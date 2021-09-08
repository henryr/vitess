/*
Copyright 2020 The Vitess Authors.

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

package planbuilder

import (
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/engine"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/abstract"
	"vitess.io/vitess/go/vt/vtgate/semantics"
)

var _ selectPlanner = gen4Planner

func gen4Planner(_ string) func(sqlparser.Statement, *sqlparser.ReservedVars, ContextVSchema) (engine.Primitive, error) {
	return func(stmt sqlparser.Statement, reservedVars *sqlparser.ReservedVars, vschema ContextVSchema) (engine.Primitive, error) {
		selStatement, ok := stmt.(sqlparser.SelectStatement)
		if !ok {
			return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "%T not yet supported", stmt)
		}

		sel, isSel := selStatement.(*sqlparser.Select)
		if isSel {
			// handle dual table for processing at vtgate.
			p, err := handleDualSelects(sel, vschema)
			if err != nil || p != nil {
				return p, err
			}
		}

		getPlan := func(selStatement sqlparser.SelectStatement) (logicalPlan, error) {
			return newBuildSelectPlan(selStatement, reservedVars, vschema)
		}

		plan, err := getPlan(selStatement)
		if err != nil {
			return nil, err
		}

		if shouldRetryWithCNFRewriting(plan) {
			// by transforming the predicates to CNF, the planner will sometimes find better plans
			primitive := gen4CNFRewrite(stmt, getPlan)
			if primitive != nil {
				return primitive, nil
			}
		}
		return plan.Primitive(), nil
	}
}

func gen4CNFRewrite(stmt sqlparser.Statement, getPlan func(selStatement sqlparser.SelectStatement) (logicalPlan, error)) engine.Primitive {
	rewritten, isSel := sqlparser.RewriteToCNF(stmt).(sqlparser.SelectStatement)
	if !isSel {
		// Fail-safe code, should never happen
		return nil
	}
	plan2, err := getPlan(rewritten)
	if err == nil && !shouldRetryWithCNFRewriting(plan2) {
		// we only use this new plan if it's better than the old one we got
		return plan2.Primitive()
	}
	return nil
}

func newBuildSelectPlan(selStmt sqlparser.SelectStatement, reservedVars *sqlparser.ReservedVars, vschema ContextVSchema) (logicalPlan, error) {
	ksName := ""
	if ks, _ := vschema.DefaultKeyspace(); ks != nil {
		ksName = ks.Name
	}
	semTable, err := semantics.Analyze(selStmt, ksName, vschema, starRewrite)
	if err != nil {
		return nil, err
	}

	ctx := newPlanningContext(reservedVars, semTable, vschema)
	err = queryRewrite(ctx, selStmt)
	if err != nil {
		return nil, err
	}

	opTree, err := abstract.CreateOperatorFromAST(selStmt, semTable)
	if err != nil {
		return nil, err
	}

	tree, err := optimizeQuery(ctx, opTree)
	if err != nil {
		return nil, err
	}

	plan, err := transformToLogicalPlan(ctx, tree)
	if err != nil {
		return nil, err
	}

	sel, isSel := selStmt.(*sqlparser.Select)
	// We do not need to call planHorizon for union queries.
	// We would have already executed this for the select statements while transforming to logical plans.
	if isSel {
		plan, err = planHorizon(ctx, plan, sel)
		if err != nil {
			return nil, err
		}

		if err := setMiscFunc(plan, sel); err != nil {
			return nil, err
		}
	}

	if err := plan.WireupGen4(semTable); err != nil {
		return nil, err
	}

	directives := sqlparser.ExtractCommentDirectives(sqlparser.GetFirstSelect(selStmt).Comments)
	if directives.IsSet(sqlparser.DirectiveScatterErrorsAsWarnings) {
		_, _ = visit(plan, func(logicalPlan logicalPlan) (bool, logicalPlan, error) {
			switch plan := logicalPlan.(type) {
			case *route:
				plan.eroute.ScatterErrorsAsWarnings = true
			}
			return true, logicalPlan, nil
		})
	}

	return plan, nil
}

func newPlanningContext(reservedVars *sqlparser.ReservedVars, semTable *semantics.SemTable, vschema ContextVSchema) planningContext {
	ctx := planningContext{
		reservedVars: reservedVars,
		semTable:     semTable,
		vschema:      vschema,
		sqToReplace:  map[string]*sqlparser.Select{},
	}
	return ctx
}

func planLimit(limit *sqlparser.Limit, plan logicalPlan) (logicalPlan, error) {
	if limit == nil {
		return plan, nil
	}
	rb, ok := plan.(*route)
	if ok && rb.isSingleShard() {
		rb.SetLimit(limit)
		return plan, nil
	}

	lPlan, err := createLimit(plan, limit)
	if err != nil {
		return nil, err
	}

	// visit does not modify the plan.
	_, err = visit(lPlan, setUpperLimit)
	if err != nil {
		return nil, err
	}
	return lPlan, nil
}

func checkUnsupportedConstructs(sel *sqlparser.Select) error {
	if sel.SQLCalcFoundRows {
		return semantics.Gen4NotSupportedF("sql_calc_found_rows")
	}
	return nil
}

func planHorizon(ctx planningContext, plan logicalPlan, in sqlparser.SelectStatement) (logicalPlan, error) {
	sel, isSel := in.(*sqlparser.Select)
	if isSel {
		hp := horizonPlanning{
			sel: sel,
		}

		replaceSubQuery(ctx.sqToReplace, sel)
		var err error
		plan, err = hp.planHorizon(ctx, plan)
		if err != nil {
			return nil, err
		}
		plan, err = planLimit(sel.Limit, plan)
		if err != nil {
			return nil, err
		}
	}

	return plan, nil

}