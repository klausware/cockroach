// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package optbuilder

import (
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"
)

// constructProjectForScope constructs a projection if it will result in a different
// set of columns than its input. Either way, it updates projectionsScope.group
// with the output memo group ID.
func (b *Builder) constructProjectForScope(inScope, projectionsScope *scope) {
	// Don't add an unnecessary "pass through" project.
	if projectionsScope.hasSameColumns(inScope) {
		projectionsScope.group = inScope.group
	} else {
		projectionsScope.group = b.constructProject(inScope.group, projectionsScope.cols)
	}
}

func (b *Builder) constructProject(input memo.GroupID, cols []scopeColumn) memo.GroupID {
	def := memo.ProjectionsOpDef{
		SynthesizedCols: make(opt.ColList, 0, len(cols)),
	}

	groupList := make([]memo.GroupID, 0, len(cols))

	// Deduplicate the columns; we only need to project each column once.
	colSet := opt.ColSet{}
	for i := range cols {
		id, group := cols[i].id, cols[i].group
		if !colSet.Contains(int(id)) {
			if group == 0 {
				def.PassthroughCols.Add(int(id))
			} else {
				def.SynthesizedCols = append(def.SynthesizedCols, id)
				groupList = append(groupList, group)
			}
			colSet.Add(int(id))
		}
	}

	return b.factory.ConstructProject(
		input,
		b.factory.ConstructProjections(
			b.factory.InternList(groupList),
			b.factory.InternProjectionsOpDef(&def),
		),
	)
}

// buildProjectionList builds a set of memo groups that represent the given
// list of select expressions.
//
// See Builder.buildStmt for a description of the remaining input values.
//
// As a side-effect, the appropriate scopes are updated with aggregations
// (scope.groupby.aggs)
func (b *Builder) buildProjectionList(selects tree.SelectExprs, inScope *scope, outScope *scope) {
	for _, e := range selects {
		// Pre-normalize any VarName so the work is not done twice below.
		if err := e.NormalizeTopLevelVarName(); err != nil {
			panic(builderError{err})
		}

		// Special handling for "*" and "<table>.*".
		if v, ok := e.Expr.(tree.VarName); ok {
			switch v.(type) {
			case tree.UnqualifiedStar, *tree.AllColumnsSelector:
				if e.As != "" {
					panic(builderError{pgerror.NewErrorf(pgerror.CodeSyntaxError,
						"%q cannot be aliased", tree.ErrString(v))})
				}

				exprs := b.expandStar(e.Expr, inScope)
				for _, e := range exprs {
					b.buildScalarProjection(e, "", inScope, outScope)
				}
				continue
			}
		}

		// Output column names should exactly match the original expression, so we
		// have to determine the output column name before we perform type
		// checking.
		label := b.getColName(e)
		texpr := inScope.resolveType(e.Expr, types.Any)
		b.buildScalarProjection(texpr, label, inScope, outScope)
	}
}

// getColName returns the output column name for a projection expression.
// It assumes that top-level variable names have already been normalized via a
// call to NormalizeTopLevelVarName().
// NB: This function is copied from sql/getRenderColName() in sql/render.go.
func (b *Builder) getColName(expr tree.SelectExpr) string {
	if expr.As != "" {
		return string(expr.As)
	}

	switch t := expr.Expr.(type) {
	case *tree.ColumnItem:
		// If the expression designates a column, try to reuse that column's name
		// as projection name.
		return t.Column()

	case *tree.FuncExpr:
		// Special case for projecting builtin functions: the column name for an
		// otherwise un-named builtin output column is just the name of the builtin.
		fd, err := t.Func.Resolve(b.semaCtx.SearchPath)
		if err != nil {
			panic(builderError{err})
		}
		return fd.Name

	case *tree.ColumnAccessExpr:
		// Special case for projecting a column accessor.
		if t.Star {
			// TODO(bram): remove this, this case should never happen once (srf).* is
			// correctly implemented.
			return t.Expr.String()
		}
		return t.ColName
	}

	return expr.Expr.String()
}

// buildScalarProjection builds a set of memo groups that represent a scalar
// expression, and then projects a new output column (either passthrough or
// synthesized) in outScope having that expression as its value.
//
// texpr   The given scalar expression.
// label   If a new column is synthesized, it will be labeled with this string.
//         For example, the query `SELECT (x + 1) AS "x_incr" FROM t` has a
//         projection with a synthesized column "x_incr".
//
// The return value corresponds to the new column which has been created for
// this scalar expression.
//
// See Builder.buildStmt for a description of the remaining input values.
func (b *Builder) buildScalarProjection(
	texpr tree.TypedExpr, label string, inScope, outScope *scope,
) *scopeColumn {
	b.buildScalarHelper(texpr, label, inScope, outScope)
	return &outScope.cols[len(outScope.cols)-1]
}

// finishBuildScalar completes construction of a new scalar expression. If
// outScope is nil, then finishBuildScalar returns the result memo group, which
// can be nested within the larger expression being built. If outScope is not
// nil, then finishBuildScalar synthesizes a new output column in outScope with
// the expression as its value.
//
// texpr     The given scalar expression. The expression is any scalar
//           expression except for a bare variable or aggregate (those are
//           handled separately in buildVariableProjection and
//           buildFunction).
// group     The memo group that has already been built for the given
//           expression.
// label     If a new column is synthesized, it will be labeled with this
//           string.
//
// See Builder.buildStmt for a description of the remaining input and return
// values.
func (b *Builder) finishBuildScalar(
	texpr tree.TypedExpr, group memo.GroupID, label string, inScope, outScope *scope,
) (out memo.GroupID) {
	if outScope == nil {
		return group
	}

	// Avoid synthesizing a new column if possible.
	if col := outScope.findExistingCol(texpr); col != nil {
		col = outScope.appendColumn(col, label)
		col.group = group
		return
	}

	b.synthesizeColumn(outScope, label, texpr.ResolvedType(), texpr, group)
	return group
}

// finishBuildScalarRef constructs a reference to the given column. If outScope
// is nil, then finishBuildScalarRef returns a Variable expression that refers
// to the column. This expression can be nested within the larger expression
// being constructed. If outScope is not nil, then finishBuildScalarRef adds the
// column as a passthrough column to outScope.
//
// col     Column containing the scalar expression that's been referenced.
// label   If passthrough column is added, it will optionally be labeled with
//         this string (if not empty).
//
// See Builder.buildStmt for a description of the remaining input and return
// values.
func (b *Builder) finishBuildScalarRef(
	col *scopeColumn, label string, inScope, outScope *scope,
) (out memo.GroupID) {
	// If this is not a projection context, then wrap the column reference with
	// a Variable expression that can be embedded in outer expression(s).
	if outScope == nil {
		return b.factory.ConstructVariable(b.factory.InternColumnID(col.id))
	}

	// Project the column, which has the side effect of making it visible.
	col = outScope.appendColumn(col, label)
	col.hidden = false
	return col.group
}
