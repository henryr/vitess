package grpcvtctldserver

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"
	"text/template"
	"time"

	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/sync2"
	"vitess.io/vitess/go/vt/concurrency"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/mysqlctl/tmutils"
	tabletmanagerdatapb "vitess.io/vitess/go/vt/proto/tabletmanagerdata"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	"vitess.io/vitess/go/vt/proto/vtctldata"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/topoproto"
)

// CopySchemaShard copies the schema from a source tablet to the
// specified shard.  The schema is applied directly on the master of
// the destination shard, and is propagated to the replicas through
// binlogs.
func (s *VtctldServer) CopySchemaShard(ctx context.Context, sourceTabletAlias *topodatapb.TabletAlias, tables, excludeTables []string, includeViews bool, destKeyspace, destShard string, waitReplicasTimeout time.Duration, skipVerify bool) error {
	destShardInfo, err := s.ts.GetShard(ctx, destKeyspace, destShard)
	if err != nil {
		return fmt.Errorf("GetShard(%v, %v) failed: %v", destKeyspace, destShard, err)
	}

	if destShardInfo.Shard.PrimaryAlias == nil {
		return fmt.Errorf("no master in shard record %v/%v. Consider to run 'vtctl InitShardMaster' in case of a new shard or to reparent the shard to fix the topology data", destKeyspace, destShard)
	}

	err = s.copyShardMetadata(ctx, sourceTabletAlias, destShardInfo.Shard.PrimaryAlias)
	if err != nil {
		return fmt.Errorf("copyShardMetadata(%v, %v) failed: %v", sourceTabletAlias, destShardInfo.Shard.PrimaryAlias, err)
	}

	diffs, err := s.compareSchemas(ctx, sourceTabletAlias, destShardInfo.Shard.PrimaryAlias, tables, excludeTables, includeViews)
	if err != nil {
		return fmt.Errorf("CopySchemaShard failed because schemas could not be compared initially: %v", err)
	}
	if diffs == nil {
		// Return early because dest has already the same schema as source.
		return nil
	}
	req := &vtctldata.GetSchemaRequest{
		TabletAlias:   sourceTabletAlias,
		ExcludeTables: excludeTables,
		Tables:        tables,
		IncludeViews:  includeViews,
	}
	sourceSd, err := s.GetSchema(ctx, req)
	if err != nil {
		return fmt.Errorf("GetSchema(%v, %v, %v, %v) failed: %v", sourceTabletAlias, tables, excludeTables, includeViews, err)
	}
	createSQL := tmutils.SchemaDefinitionToSQLStrings(sourceSd.GetSchema())
	destTabletInfo, err := s.ts.GetTablet(ctx, destShardInfo.Shard.PrimaryAlias)
	if err != nil {
		return fmt.Errorf("GetTablet(%v) failed: %v", destShardInfo.Shard.PrimaryAlias, err)
	}
	for i, sqlLine := range createSQL {
		err = s.applySQLShard(ctx, destTabletInfo, sqlLine, i == len(createSQL)-1)
		if err != nil {
			return fmt.Errorf("creating a table failed."+
				" Most likely some tables already exist on the destination and differ from the source."+
				" Please remove all to be copied tables from the destination manually and run this command again."+
				" Full error: %v", err)
		}
	}

	// Remember the replication position after all the above were applied.
	destMasterPos, err := s.tmc.MasterPosition(ctx, destTabletInfo.Tablet)
	if err != nil {
		return fmt.Errorf("CopySchemaShard: can't get replication position after schema applied: %v", err)
	}

	// Although the copy was successful, we have to verify it to catch the case
	// where the database already existed on the destination, but with different
	// options e.g. a different character set.
	// In that case, MySQL would have skipped our CREATE DATABASE IF NOT EXISTS
	// statement. We want to fail early in this case because vtworker SplitDiff
	// fails in case of such an inconsistency as well.
	if !skipVerify {
		diffs, err = s.compareSchemas(ctx, sourceTabletAlias, destShardInfo.Shard.PrimaryAlias, tables, excludeTables, includeViews)
		if err != nil {
			return fmt.Errorf("CopySchemaShard failed because schemas could not be compared finally: %v", err)
		}
		if diffs != nil {
			return fmt.Errorf("CopySchemaShard was not successful because the schemas between the two tablets %v and %v differ: %v", sourceTabletAlias, destShardInfo.Shard.PrimaryAlias, diffs)
		}
	}

	// Notify Replicass to reload schema. This is best-effort.
	concurrency := sync2.NewSemaphore(10, 0)
	reloadCtx, cancel := context.WithTimeout(ctx, waitReplicasTimeout)
	defer cancel()
	s.ReloadSchemaShard(reloadCtx, destKeyspace, destShard, destMasterPos, concurrency, true /* includeMaster */)
	return nil
}

// copyShardMetadata copies contents of _vt.shard_metadata table from the source
// tablet to the destination tablet. It's assumed that destination tablet is a
// master and binlogging is not turned off when INSERT statements are executed.
func (s *VtctldServer) copyShardMetadata(ctx context.Context, srcTabletAlias *topodatapb.TabletAlias, destTabletAlias *topodatapb.TabletAlias) error {
	sql := "SELECT 1 FROM information_schema.tables WHERE table_schema = '_vt' AND table_name = 'shard_metadata'"

	req := &vtctldata.GetTabletRequest{
		TabletAlias: srcTabletAlias,
	}

	getTabletResp, err := s.GetTablet(ctx, req)
	if err != nil {
		return fmt.Errorf("GetTablet(%v) failed: %v", req.TabletAlias, err)
	}
	presenceResult, err := s.tmc.ExecuteFetchAsDba(ctx, getTabletResp.GetTablet(), false, []byte(sql), 1, false, false)
	if err != nil {
		return fmt.Errorf("ExecuteFetchAsDba(%v, %v, 1, false, false) failed: %v", srcTabletAlias, sql, err)
	}
	if len(presenceResult.Rows) == 0 {
		log.Infof("_vt.shard_metadata doesn't exist on the source tablet %v, skipping its copy.", topoproto.TabletAliasString(srcTabletAlias))
		return nil
	}

	// TODO: 100 may be too low here for row limit
	sql = "SELECT db_name, name, value FROM _vt.shard_metadata"
	dataProto, err := s.tmc.ExecuteFetchAsDba(ctx, getTabletResp.GetTablet(), false, []byte(sql), 100, false, false)
	if err != nil {
		return fmt.Errorf("ExecuteFetchAsDba(%v, %v, 100, false, false) failed: %v", srcTabletAlias, sql, err)
	}
	data := sqltypes.Proto3ToResult(dataProto)

	req = &vtctldata.GetTabletRequest{
		TabletAlias: destTabletAlias,
	}

	getTabletResp, err = s.GetTablet(ctx, req)
	if err != nil {
		return fmt.Errorf("GetTablet(%v) failed: %v", req.TabletAlias, err)
	}

	for _, row := range data.Rows {
		dbName := row[0]
		name := row[1]
		value := row[2]
		queryBuf := bytes.Buffer{}
		queryBuf.WriteString("INSERT INTO _vt.shard_metadata (db_name, name, value) VALUES (")
		dbName.EncodeSQL(&queryBuf)
		queryBuf.WriteByte(',')
		name.EncodeSQL(&queryBuf)
		queryBuf.WriteByte(',')
		value.EncodeSQL(&queryBuf)
		queryBuf.WriteString(") ON DUPLICATE KEY UPDATE value = ")
		value.EncodeSQL(&queryBuf)

		_, err := s.tmc.ExecuteFetchAsDba(ctx, getTabletResp.GetTablet(), false, []byte(queryBuf.String()), 0, false, false)
		if err != nil {
			return fmt.Errorf("ExecuteFetchAsDba(%v, %v, 0, false, false) failed: %v", destTabletAlias, queryBuf.String(), err)
		}
	}
	return nil
}

// compareSchemas returns nil if the schema of the two tablets referenced by
// "sourceAlias" and "destAlias" are identical. Otherwise, the difference is
// returned as []string.
func (s *VtctldServer) compareSchemas(ctx context.Context, sourceAlias, destAlias *topodatapb.TabletAlias, tables, excludeTables []string, includeViews bool) ([]string, error) {
	req := &vtctldata.GetSchemaRequest{
		TabletAlias:   sourceAlias,
		ExcludeTables: excludeTables,
		Tables:        tables,
		IncludeViews:  includeViews,
	}
	sourceSd, err := s.GetSchema(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get schema from tablet %v. err: %v", sourceAlias, err)
	}
	req = &vtctldata.GetSchemaRequest{
		TabletAlias:   destAlias,
		ExcludeTables: excludeTables,
		Tables:        tables,
		IncludeViews:  includeViews,
	}
	destSd, err := s.GetSchema(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get schema from tablet %v. err: %v", destAlias, err)
	}
	return tmutils.DiffSchemaToArray("source", sourceSd.GetSchema(), "dest", destSd.GetSchema()), nil
}

// applySQLShard applies a given SQL change on a given tablet alias. It allows executing arbitrary
// SQL statements, but doesn't return any results, so it's only useful for SQL statements
// that would be run for their effects (e.g., CREATE).
// It works by applying the SQL statement on the shard's master tablet with replication turned on.
// Thus it should be used only for changes that can be applied on a live instance without causing issues;
// it shouldn't be used for anything that will require a pivot.
// The SQL statement string is expected to have {{.DatabaseName}} in place of the actual db name.
func (s *VtctldServer) applySQLShard(ctx context.Context, tabletInfo *topo.TabletInfo, change string, reloadSchema bool) error {
	filledChange, err := fillStringTemplate(change, map[string]string{"DatabaseName": tabletInfo.DbName()})
	if err != nil {
		return fmt.Errorf("fillStringTemplate failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Need to make sure that we enable binlog, since we're only applying the statement on masters.
	_, err = s.tmc.ExecuteFetchAsDba(ctx, tabletInfo.Tablet, false, []byte(filledChange), 0, false, reloadSchema)
	return err
}

// fillStringTemplate returns the string template filled
func fillStringTemplate(tmpl string, vars interface{}) (string, error) {
	myTemplate := template.Must(template.New("").Parse(tmpl))
	data := new(bytes.Buffer)
	if err := myTemplate.Execute(data, vars); err != nil {
		return "", err
	}
	return data.String(), nil
}

// ReloadSchemaShard reloads the schema for all replica tablets in a shard,
// after they reach a given replication position (empty pos means immediate).
// In general, we don't always expect all replicas to be ready to reload,
// and the periodic schema reload makes them self-healing anyway.
// So we do this on a best-effort basis, and log warnings for any tablets
// that fail to reload within the context deadline.
func (s *VtctldServer) ReloadSchemaShard(ctx context.Context, keyspace, shard, replicationPos string, concurrency *sync2.Semaphore, includeMaster bool) {
	tablets, err := s.ts.GetTabletMapForShard(ctx, keyspace, shard)
	switch {
	case topo.IsErrType(err, topo.PartialResult):
		// We got a partial result. Do what we can, but warn
		// that some may be missed.
		s.logger.Warningf("ReloadSchemaShard(%v/%v) got a partial tablet list. Some tablets may not have schema reloaded (use vtctl ReloadSchema to fix individual tablets)", keyspace, shard)
	case err == nil:
		// Good case, keep going too.
	default:
		// This is best-effort, so just log it and move on.
		s.logger.Warningf("ReloadSchemaShard(%v/%v) failed to load tablet list, will not reload schema (use vtctl ReloadSchemaShard to try again): %v", keyspace, shard, err)
		return
	}

	var wg sync.WaitGroup
	for _, ti := range tablets {
		if !includeMaster && ti.Type == topodatapb.TabletType_MASTER {
			// We don't need to reload on the master
			// because we assume ExecuteFetchAsDba()
			// already did that.
			continue
		}

		wg.Add(1)
		go func(tablet *topodatapb.Tablet) {
			defer wg.Done()
			concurrency.Acquire()
			defer concurrency.Release()
			pos := replicationPos
			// Master is always up-to-date. So, don't wait for position.
			if tablet.Type == topodatapb.TabletType_MASTER {
				pos = ""
			}
			if err := s.tmc.ReloadSchema(ctx, tablet, pos); err != nil {
				s.logger.Warningf(
					"Failed to reload schema on replica tablet %v in %v/%v (use vtctl ReloadSchema to try again): %v",
					topoproto.TabletAliasString(tablet.Alias), keyspace, shard, err)
			}
		}(ti.Tablet)
	}
	wg.Wait()
}

// ValidateSchemaKeyspace will diff the schema from all the tablets in
// the keyspace.
func (s *VtctldServer) ValidateSchemaKeyspace(ctx context.Context, keyspace string, excludeTables []string, includeViews, skipNoMaster bool, includeVSchema bool) error {
	// find all the shards
	shards, err := s.ts.GetShardNames(ctx, keyspace)
	if err != nil {
		return fmt.Errorf("GetShardNames(%v) failed: %v", keyspace, err)
	}

	// corner cases
	if len(shards) == 0 {
		return fmt.Errorf("no shards in keyspace %v", keyspace)
	}
	sort.Strings(shards)
	if len(shards) == 1 {
		return s.ValidateSchemaShard(ctx, keyspace, shards[0], excludeTables, includeViews, includeVSchema)
	}

	var referenceSchema *tabletmanagerdatapb.SchemaDefinition
	var referenceAlias *topodatapb.TabletAlias

	// then diff with all other tablets everywhere
	er := concurrency.AllErrorRecorder{}
	wg := sync.WaitGroup{}

	// If we are checking against the vschema then all shards
	// should just be validated individually against it
	if includeVSchema {
		err := s.ValidateVSchema(ctx, keyspace, shards, excludeTables, includeViews)
		if err != nil {
			return err
		}
	}

	// then diffs all tablets in the other shards
	for _, shard := range shards[0:] {
		si, err := s.ts.GetShard(ctx, keyspace, shard)
		if err != nil {
			er.RecordError(fmt.Errorf("GetShard(%v, %v) failed: %v", keyspace, shard, err))
			continue
		}

		if !si.HasPrimary() {
			if !skipNoMaster {
				er.RecordError(fmt.Errorf("no primary in shard %v/%v", keyspace, shard))
			}
			continue
		}

		if referenceSchema == nil {
			referenceAlias = si.Shard.PrimaryAlias
			log.Infof("Gathering schema for reference primary %v", topoproto.TabletAliasString(referenceAlias))

			req := &vtctldata.GetTabletRequest{
				TabletAlias: referenceAlias,
			}

			getTabletResp, err := s.GetTablet(ctx, req)
			if err != nil {
				return fmt.Errorf("GetTablet(%v) failed: %v", referenceAlias, err)
			}

			referenceSchema, err = s.tmc.GetSchema(ctx, getTabletResp.GetTablet(), nil, excludeTables, includeViews)
			if err != nil {
				return fmt.Errorf("GetSchema(%v, nil, %v, %v) failed: %v", referenceAlias, excludeTables, includeViews, err)
			}
		}

		aliases, err := s.ts.FindAllTabletAliasesInShard(ctx, keyspace, shard)
		if err != nil {
			er.RecordError(fmt.Errorf("FindAllTabletAliasesInShard(%v, %v) failed: %v", keyspace, shard, err))
			continue
		}

		for _, alias := range aliases {
			// Don't diff schemas for self
			if referenceAlias == alias {
				continue
			}
			wg.Add(1)
			go s.diffSchema(ctx, referenceSchema, referenceAlias, alias, excludeTables, includeViews, &wg, &er)
		}
	}
	wg.Wait()
	if er.HasErrors() {
		return fmt.Errorf("schema diffs: %v", er.Error().Error())
	}
	return nil
}

// ValidateSchemaShard will diff the schema from all the tablets in the shard.
func (s *VtctldServer) ValidateSchemaShard(ctx context.Context, keyspace, shard string, excludeTables []string, includeViews bool, includeVSchema bool) error {
	si, err := s.ts.GetShard(ctx, keyspace, shard)
	if err != nil {
		return fmt.Errorf("GetShard(%v, %v) failed: %v", keyspace, shard, err)
	}

	// get schema from the master, or error
	if !si.HasPrimary() {
		return fmt.Errorf("no primary in shard %v/%v", keyspace, shard)
	}
	log.Infof("Gathering schema for primary %v", topoproto.TabletAliasString(si.Shard.PrimaryAlias))

	req := &vtctldata.GetTabletRequest{
		TabletAlias: si.Shard.PrimaryAlias,
	}

	getTabletResponse, err := s.GetTablet(ctx, req)
	if err != nil {
		return fmt.Errorf("GetTablet(%v) failed: %v", req.TabletAlias, err)
	}

	masterSchema, err := s.tmc.GetSchema(ctx, getTabletResponse.Tablet, nil, excludeTables, includeViews)
	if err != nil {
		return fmt.Errorf("GetSchema(%v, nil, %v, %v) failed: %v", si.Shard.PrimaryAlias, excludeTables, includeViews, err)
	}

	if includeVSchema {
		err := s.ValidateVSchema(ctx, keyspace, []string{shard}, excludeTables, includeViews)
		if err != nil {
			return err
		}
	}

	// read all the aliases in the shard, that is all tablets that are
	// replicating from the master
	aliases, err := s.ts.FindAllTabletAliasesInShard(ctx, keyspace, shard)
	if err != nil {
		return fmt.Errorf("FindAllTabletAliasesInShard(%v, %v) failed: %v", keyspace, shard, err)
	}

	// then diff with all replicas
	er := concurrency.AllErrorRecorder{}
	wg := sync.WaitGroup{}
	for _, alias := range aliases {
		if topoproto.TabletAliasEqual(alias, si.Shard.PrimaryAlias) {
			continue
		}

		wg.Add(1)
		go s.diffSchema(ctx, masterSchema, si.Shard.PrimaryAlias, alias, excludeTables, includeViews, &wg, &er)
	}
	wg.Wait()
	if er.HasErrors() {
		return fmt.Errorf("schema diffs: %v", er.Error().Error())
	}
	return nil
}

// ValidateVSchema compares the schema of each primary tablet in "keyspace/shards..." to the vschema and errs if there are differences
func (s *VtctldServer) ValidateVSchema(ctx context.Context, keyspace string, shards []string, excludeTables []string, includeViews bool) error {
	vschm, err := s.ts.GetVSchema(ctx, keyspace)
	if err != nil {
		return fmt.Errorf("GetVSchema(%s) failed: %v", keyspace, err)
	}

	shardFailures := concurrency.AllErrorRecorder{}
	var wg sync.WaitGroup
	wg.Add(len(shards))

	for _, shard := range shards {
		go func(shard string) {
			defer wg.Done()
			notFoundTables := []string{}
			si, err := s.ts.GetShard(ctx, keyspace, shard)
			if err != nil {
				shardFailures.RecordError(fmt.Errorf("GetShard(%v, %v) failed: %v", keyspace, shard, err))
				return
			}

			primaryTablet, err := s.ts.GetTablet(ctx, si.Shard.PrimaryAlias)
			if err != nil {
				shardFailures.RecordError(fmt.Errorf("GetTablet(%v) failed: %v", si.Shard.PrimaryAlias, err))
				return
			}

			masterSchema, err := s.tmc.GetSchema(ctx, primaryTablet.Tablet, nil, excludeTables, includeViews)
			if err != nil {
				shardFailures.RecordError(fmt.Errorf("GetSchema(%s, nil, %v, %v) (%v/%v) failed: %v", si.Shard.PrimaryAlias.String(),
					excludeTables, includeViews, keyspace, shard, err,
				))
				return
			}
			for _, tableDef := range masterSchema.TableDefinitions {
				if _, ok := vschm.Tables[tableDef.Name]; !ok {
					notFoundTables = append(notFoundTables, tableDef.Name)
				}
			}
			if len(notFoundTables) > 0 {
				shardFailure := fmt.Errorf("%v/%v has tables that are not in the vschema: %v", keyspace, shard, notFoundTables)
				shardFailures.RecordError(shardFailure)
			}
		}(shard)
	}
	wg.Wait()
	if shardFailures.HasErrors() {
		return fmt.Errorf("ValidateVSchema(%v, %v, %v, %v) failed: %v", keyspace, shards, excludeTables, includeViews, shardFailures.Error().Error())
	}
	return nil
}

// helper method to asynchronously diff a schema
func (s *VtctldServer) diffSchema(ctx context.Context, masterSchema *tabletmanagerdatapb.SchemaDefinition, masterTabletAlias, alias *topodatapb.TabletAlias, excludeTables []string, includeViews bool, wg *sync.WaitGroup, er concurrency.ErrorRecorder) {
	defer wg.Done()
	log.Infof("Gathering schema for %v", topoproto.TabletAliasString(alias))

	req := &vtctldata.GetTabletRequest{
		TabletAlias: alias,
	}

	getTabletResp, err := s.GetTablet(ctx, req)
	if err != nil {
		er.RecordError(fmt.Errorf("GetTablet(%v) failed: %v", alias, err))
	}

	replicaSchema, err := s.tmc.GetSchema(ctx, getTabletResp.GetTablet(), nil, excludeTables, includeViews)
	if err != nil {
		er.RecordError(fmt.Errorf("GetSchema(%v, nil, %v, %v) failed: %v", alias, excludeTables, includeViews, err))
		return
	}

	log.Infof("Diffing schema for %v", topoproto.TabletAliasString(alias))
	tmutils.DiffSchema(topoproto.TabletAliasString(masterTabletAlias), masterSchema, topoproto.TabletAliasString(alias), replicaSchema, er)
}