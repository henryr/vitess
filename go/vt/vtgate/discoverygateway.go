/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vtgate

import (
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"context"

	"vitess.io/vitess/go/stats"
	"vitess.io/vitess/go/vt/discovery"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/srvtopo"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/buffer"
	"vitess.io/vitess/go/vt/vttablet/queryservice"

	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
)

var (
	routeReplicaToRdonly = flag.Bool("gateway_route_replica_to_rdonly", false, "route REPLICA queries to RDONLY tablets as well as REPLICA tablets")
)

const (
	// GatewayImplementationDiscovery defines the string value used as the implementation key for DiscoveryGateway
	GatewayImplementationDiscovery = "discoverygateway"
)

//UsingLegacyGateway returns true when legacy
func UsingLegacyGateway() bool {
	return *GatewayImplementation == GatewayImplementationDiscovery
}

func init() {
	RegisterGatewayCreator(GatewayImplementationDiscovery, createDiscoveryGateway)
}

// DiscoveryGateway is not the default Gateway implementation anymore.
// This implementation uses the legacy healthcheck module.
type DiscoveryGateway struct {
	queryservice.QueryService
	hc            discovery.LegacyHealthCheck
	tsc           *discovery.LegacyTabletStatsCache
	srvTopoServer srvtopo.Server
	localCell     string
	retryCount    int

	// tabletsWatchers contains a list of all the watchers we use.
	// We create one per cell.
	tabletsWatchers []*discovery.LegacyTopologyWatcher

	// mu protects the fields of this group.
	mu sync.RWMutex
	// statusAggregators is a map indexed by the key
	// keyspace/shard/tablet_type.
	statusAggregators map[string]*TabletStatusAggregator

	// buffer, if enabled, buffers requests during a detected MASTER failover.
	buffer *buffer.Buffer
}

//TabletsCacheStatus is not implemented for this struct
func (dg *DiscoveryGateway) TabletsCacheStatus() discovery.TabletsCacheStatusList {
	return nil
}

var _ Gateway = (*DiscoveryGateway)(nil)

func createDiscoveryGateway(ctx context.Context, hc discovery.LegacyHealthCheck, serv srvtopo.Server, cell string, retryCount int) Gateway {
	return NewDiscoveryGateway(ctx, hc, serv, cell, retryCount)
}

// NewDiscoveryGateway creates a new DiscoveryGateway using the provided healthcheck and toposerver.
// cell is the cell where the gateway is located a.k.a localCell.
// This gateway can route to MASTER in any cell provided by the cells_to_watch command line argument.
// Other tablet type requests (REPLICA/RDONLY) are only routed to tablets in the same cell.
func NewDiscoveryGateway(ctx context.Context, hc discovery.LegacyHealthCheck, serv srvtopo.Server, cell string, retryCount int) *DiscoveryGateway {
	var topoServer *topo.Server
	if serv != nil {
		var err error
		topoServer, err = serv.GetTopoServer()
		if err != nil {
			log.Exitf("Unable to create new discoverygateway: %v", err)
		}
	}

	dg := &DiscoveryGateway{
		hc:                hc,
		tsc:               discovery.NewTabletStatsCacheDoNotSetListener(topoServer, cell),
		srvTopoServer:     serv,
		localCell:         cell,
		retryCount:        retryCount,
		tabletsWatchers:   make([]*discovery.LegacyTopologyWatcher, 0, 1),
		statusAggregators: make(map[string]*TabletStatusAggregator),
		buffer:            buffer.New(),
	}

	// Set listener which will update LegacyTabletStatsCache and MasterBuffer.
	// We set sendDownEvents=true because it's required by LegacyTabletStatsCache.
	hc.SetListener(dg, true /* sendDownEvents */)

	cells := *CellsToWatch
	log.Infof("loading tablets for cells: %v", cells)
	for _, c := range strings.Split(cells, ",") {
		if c == "" {
			continue
		}
		var recorder discovery.LegacyTabletRecorder = dg.hc
		if len(discovery.TabletFilters) > 0 {
			if discovery.FilteringKeyspaces() {
				log.Exitf("Only one of -keyspaces_to_watch and -tablet_filters may be specified at a time")
			}

			fbs, err := discovery.NewLegacyFilterByShard(recorder, discovery.TabletFilters)
			if err != nil {
				log.Exitf("Cannot parse tablet_filters parameter: %v", err)
			}
			recorder = fbs
		} else if discovery.FilteringKeyspaces() {
			recorder = discovery.NewLegacyFilterByKeyspace(recorder, discovery.KeyspacesToWatch)
		}

		ctw := discovery.NewLegacyCellTabletsWatcher(ctx, topoServer, recorder, c, *discovery.RefreshInterval, *discovery.RefreshKnownTablets, *discovery.TopoReadConcurrency)
		dg.tabletsWatchers = append(dg.tabletsWatchers, ctw)
	}
	dg.QueryService = queryservice.Wrap(nil, dg.withRetry)
	return dg
}

// RegisterStats registers the stats to export the lag since the last refresh
// and the checksum of the topology
func (dg *DiscoveryGateway) RegisterStats() {
	stats.NewGaugeDurationFunc(
		"TopologyWatcherMaxRefreshLag",
		"maximum time since the topology watcher refreshed a cell",
		dg.topologyWatcherMaxRefreshLag,
	)

	stats.NewGaugeFunc(
		"TopologyWatcherChecksum",
		"crc32 checksum of the topology watcher state",
		dg.topologyWatcherChecksum,
	)
}

// topologyWatcherMaxRefreshLag returns the maximum lag since the watched
// cells were refreshed from the topo server
func (dg *DiscoveryGateway) topologyWatcherMaxRefreshLag() time.Duration {
	var lag time.Duration
	for _, tw := range dg.tabletsWatchers {
		cellLag := tw.RefreshLag()
		if cellLag > lag {
			lag = cellLag
		}
	}
	return lag
}

// topologyWatcherChecksum returns a checksum of the topology watcher state
func (dg *DiscoveryGateway) topologyWatcherChecksum() int64 {
	var checksum int64
	for _, tw := range dg.tabletsWatchers {
		checksum = checksum ^ int64(tw.TopoChecksum())
	}
	return checksum
}

// StatsUpdate forwards LegacyHealthCheck updates to LegacyTabletStatsCache and MasterBuffer.
// It is part of the discovery.LegacyHealthCheckStatsListener interface.
func (dg *DiscoveryGateway) StatsUpdate(ts *discovery.LegacyTabletStats) {
	dg.tsc.StatsUpdate(ts)

	if ts.Target.TabletType == topodatapb.TabletType_MASTER {
		dg.buffer.StatsUpdate(ts)
	}
}

// WaitForTablets is part of the gateway.Gateway interface.
func (dg *DiscoveryGateway) WaitForTablets(ctx context.Context, tabletTypesToWait []topodatapb.TabletType) error {
	// Skip waiting for tablets if we are not told to do so.
	if len(tabletTypesToWait) == 0 {
		return nil
	}

	// Finds the targets to look for.
	targets, err := srvtopo.FindAllTargets(ctx, dg.srvTopoServer, dg.localCell, tabletTypesToWait)
	if err != nil {
		return err
	}

	filteredTargets := discovery.FilterTargetsByKeyspaces(discovery.KeyspacesToWatch, targets)
	return dg.tsc.WaitForAllServingTablets(ctx, filteredTargets)
}

// Close shuts down underlying connections.
// This function hides the inner implementation.
func (dg *DiscoveryGateway) Close(ctx context.Context) error {
	dg.buffer.Shutdown()
	for _, ctw := range dg.tabletsWatchers {
		ctw.Stop()
	}
	return nil
}

// CacheStatus returns a list of TabletCacheStatus per
// keyspace/shard/tablet_type.
func (dg *DiscoveryGateway) CacheStatus() TabletCacheStatusList {
	dg.mu.RLock()
	res := make(TabletCacheStatusList, 0, len(dg.statusAggregators))
	for _, aggr := range dg.statusAggregators {
		res = append(res, aggr.GetCacheStatus())
	}
	dg.mu.RUnlock()
	sort.Sort(res)
	return res
}

// withRetry gets available connections and executes the action. If there are retryable errors,
// it retries retryCount times before failing. It does not retry if the connection is in
// the middle of a transaction. While returning the error check if it maybe a result of
// a resharding event, and set the re-resolve bit and let the upper layers
// re-resolve and retry.
func (dg *DiscoveryGateway) withRetry(ctx context.Context, target *querypb.Target, unused queryservice.QueryService, name string, inTransaction bool, inner func(ctx context.Context, target *querypb.Target, conn queryservice.QueryService) (bool, error)) error {
	var err error
	invalidTablets := make(map[string]bool)

	if len(discovery.AllowedTabletTypes) > 0 {
		var match bool
		for _, allowed := range discovery.AllowedTabletTypes {
			if allowed == target.TabletType {
				match = true
				break
			}
		}
		if !match {
			return vterrors.Errorf(vtrpcpb.Code_FAILED_PRECONDITION, "requested tablet type %v is not part of the allowed tablet types for this vtgate: %+v", target.TabletType.String(), discovery.AllowedTabletTypes)
		}
	}

	bufferedOnce := false
	for i := 0; i < dg.retryCount+1; i++ {
		// Check if we should buffer MASTER queries which failed due to an ongoing
		// failover.
		// Note: We only buffer once and only "!inTransaction" queries i.e.
		// a) no transaction is necessary (e.g. critical reads) or
		// b) no transaction was created yet.
		if !bufferedOnce && !inTransaction && target.TabletType == topodatapb.TabletType_MASTER {
			// The next call blocks if we should buffer during a failover.
			retryDone, bufferErr := dg.buffer.WaitForFailoverEnd(ctx, target.Keyspace, target.Shard, err)
			if bufferErr != nil {
				// Buffering failed e.g. buffer is already full. Do not retry.
				err = vterrors.Errorf(
					vterrors.Code(bufferErr),
					"failed to automatically buffer and retry failed request during failover: %v original err (type=%T): %v",
					bufferErr, err, err)
				break
			}

			// Request may have been buffered.
			if retryDone != nil {
				// We're going to retry this request as part of a buffer drain.
				// Notify the buffer after we retried.
				defer retryDone()
				bufferedOnce = true
			}
		}

		tablets := dg.tsc.GetHealthyTabletStats(target.Keyspace, target.Shard, target.TabletType)

		// temporary hack to enable REPLICA type queries to address both REPLICA tablets and RDONLY tablets
		// original commit - https://github.com/tinyspeck/vitess/pull/166/commits/2552b4ce25a9fdb41ff07fa69f2ccf485fea83ac
		if *routeReplicaToRdonly && target.TabletType == topodatapb.TabletType_REPLICA {
			tablets = append(tablets, dg.tsc.GetHealthyTabletStats(target.Keyspace, target.Shard, topodatapb.TabletType_RDONLY)...)
		}

		if len(tablets) == 0 {
			// fail fast if there is no tablet
			err = vterrors.Errorf(vtrpcpb.Code_UNAVAILABLE, "no healthy tablet available for '%s'", target.String())
			break
		}
		shuffleTablets(dg.localCell, tablets)

		// skip tablets we tried before
		var ts *discovery.LegacyTabletStats
		for _, t := range tablets {
			if _, ok := invalidTablets[t.Key]; !ok {
				ts = &t
				break
			}
		}
		if ts == nil {
			if err == nil {
				// do not override error from last attempt.
				err = vterrors.New(vtrpcpb.Code_UNAVAILABLE, "no available connection")
			}
			break
		}

		// execute
		conn := dg.hc.GetConnection(ts.Key)
		if conn == nil {
			err = vterrors.Errorf(vtrpcpb.Code_UNAVAILABLE, "no connection for key %v tablet %+v", ts.Key, ts.Tablet)
			invalidTablets[ts.Key] = true
			continue
		}

		startTime := time.Now()
		var canRetry bool
		canRetry, err = inner(ctx, ts.Target, conn)
		dg.updateStats(target, startTime, err)
		if canRetry {
			invalidTablets[ts.Key] = true
			continue
		}
		break
	}
	return NewShardError(err, target)
}

func shuffleTablets(cell string, tablets []discovery.LegacyTabletStats) {
	sameCell, diffCell, sameCellMax := 0, 0, -1
	length := len(tablets)

	// move all same cell tablets to the front, this is O(n)
	for {
		sameCellMax = diffCell - 1
		sameCell = nextTablet(cell, tablets, sameCell, length, true)
		diffCell = nextTablet(cell, tablets, diffCell, length, false)
		// either no more diffs or no more same cells should stop the iteration
		if sameCell < 0 || diffCell < 0 {
			break
		}

		if sameCell < diffCell {
			// fast forward the `sameCell` lookup to `diffCell + 1`, `diffCell` unchanged
			sameCell = diffCell + 1
		} else {
			// sameCell > diffCell, swap needed
			tablets[sameCell], tablets[diffCell] = tablets[diffCell], tablets[sameCell]
			sameCell++
			diffCell++
		}
	}

	//shuffle in same cell tablets
	for i := sameCellMax; i > 0; i-- {
		swap := rand.Intn(i + 1)
		tablets[i], tablets[swap] = tablets[swap], tablets[i]
	}

	//shuffle in diff cell tablets
	for i, diffCellMin := length-1, sameCellMax+1; i > diffCellMin; i-- {
		swap := rand.Intn(i-sameCellMax) + diffCellMin
		tablets[i], tablets[swap] = tablets[swap], tablets[i]
	}
}

func nextTablet(cell string, tablets []discovery.LegacyTabletStats, offset, length int, sameCell bool) int {
	for ; offset < length; offset++ {
		if (tablets[offset].Tablet.Alias.Cell == cell) == sameCell {
			return offset
		}
	}
	return -1
}

func (dg *DiscoveryGateway) updateStats(target *querypb.Target, startTime time.Time, err error) {
	elapsed := time.Since(startTime)
	aggr := dg.getStatsAggregator(target)
	aggr.UpdateQueryInfo("", target.TabletType, elapsed, err != nil)
}

func (dg *DiscoveryGateway) getStatsAggregator(target *querypb.Target) *TabletStatusAggregator {
	key := fmt.Sprintf("%v/%v/%v", target.Keyspace, target.Shard, target.TabletType.String())

	// get existing aggregator
	dg.mu.RLock()
	aggr, ok := dg.statusAggregators[key]
	dg.mu.RUnlock()
	if ok {
		return aggr
	}
	// create a new one, but check again before the creation
	dg.mu.Lock()
	defer dg.mu.Unlock()
	aggr, ok = dg.statusAggregators[key]
	if ok {
		return aggr
	}
	aggr = NewTabletStatusAggregator(target.Keyspace, target.Shard, target.TabletType, key)
	dg.statusAggregators[key] = aggr
	return aggr
}

// QueryServiceByAlias satisfies the Gateway interface
func (dg *DiscoveryGateway) QueryServiceByAlias(_ *topodatapb.TabletAlias, _ *querypb.Target) (queryservice.QueryService, error) {
	return nil, vterrors.New(vtrpcpb.Code_UNIMPLEMENTED, "DiscoveryGateway does not implement QueryServiceByAlias")
}
