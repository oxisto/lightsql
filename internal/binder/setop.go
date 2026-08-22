package binder

import (
	"strconv"

	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/types"
)

// bindQueryNode binds whatever produces rows, which is a SELECT or a set
// operation over two of them.
//
// Every position that accepts a query goes through here -- a statement, a
// derived table, a subquery, the source of an INSERT -- so a set operation
// works in all of them rather than only where someone remembered to allow it.
func (b *Binder) bindQueryNode(q ast.Query) (plan.Node, error) {
	switch q := q.(type) {
	case *ast.SelectStmt:
		return b.bindSelectNode(q)
	case *ast.SetOp:
		return b.bindSetOp(q)
	default:
		return nil, pgerr.Newf(pgerr.FeatureNotSupported,
			"query type %T is not supported yet", q).At(q.Pos())
	}
}

// bindSetOp binds UNION, INTERSECT or EXCEPT.
func (b *Binder) bindSetOp(s *ast.SetOp) (plan.Node, error) {
	left, err := b.bindQueryNode(s.Left)
	if err != nil {
		return nil, err
	}
	right, err := b.bindQueryNode(s.Right)
	if err != nil {
		return nil, err
	}

	cols, err := unifyArms(s, left.Result(), right.Result())
	if err != nil {
		return nil, err
	}

	var node plan.Node = &plan.SetOp{
		Left: left, Right: right, Op: s.Op, All: s.All, Cols: cols,
	}

	if len(s.OrderBy) > 0 {
		keys, err := setOpSortKeys(s.OrderBy, cols)
		if err != nil {
			return nil, err
		}
		node = &plan.Sort{Input: node, Keys: keys}
	}
	if s.Limit != nil || s.Offset != nil {
		lim := &plan.Limit{Input: node}
		if lim.Count, err = bindRowCount(s.Limit, "LIMIT"); err != nil {
			return nil, err
		}
		if lim.Offset, err = bindRowCount(s.Offset, "OFFSET"); err != nil {
			return nil, err
		}
		node = lim
	}
	return node, nil
}

// unifyArms checks the two sides agree in shape and decides the result's types.
//
// The names come from the left arm, as they do in PostgreSQL: the right one
// contributes values, not a vocabulary.
func unifyArms(s *ast.SetOp, left, right []plan.ResultColumn) ([]plan.ResultColumn, error) {
	if len(left) != len(right) {
		return nil, pgerr.Newf(pgerr.SyntaxError,
			"each %s query must have the same number of columns", s.Op).At(s.OpPos)
	}

	out := make([]plan.ResultColumn, len(left))
	for i := range left {
		kind, err := unifyKinds(left[i].Type.Kind, right[i].Type.Kind)
		if err != nil {
			return nil, pgerr.Newf(pgerr.DatatypeMismatch,
				"%s types %s and %s cannot be matched", s.Op,
				left[i].Type.Name, right[i].Type.Name).At(s.OpPos)
		}
		// The declared type is kept when both sides agree on it, so a
		// numeric(10,2) does not lose its scale to a bare "numeric" merely by
		// passing through a union.
		typ := left[i].Type
		if typ.Kind != kind || left[i].Type.Name != right[i].Type.Name {
			typ = catalog.Type{Kind: kind, Name: kind.String()}
		}
		out[i] = plan.ResultColumn{Name: left[i].Name, Type: typ}
	}
	return out, nil
}

// unifyKinds finds the type both arms can be read as.
//
// It is deliberately narrow. PostgreSQL resolves a union's column type through
// its full type-preference machinery; this handles the cases that arise in
// practice -- an untyped NULL, two numbers of different exactness, two temporal
// kinds -- and refuses the rest rather than picking something surprising.
func unifyKinds(l, r types.Kind) (types.Kind, error) {
	switch {
	case l == r:
		return l, nil
	case l == types.KindNull:
		return r, nil
	case r == types.KindNull:
		return l, nil
	case l.IsNumeric() && r.IsNumeric():
		// The inexact kind wins, for the same reason it wins in arithmetic:
		// once one side cannot be represented exactly, calling the result
		// exact would be a lie told in the column type.
		if l == types.KindFloat || r == types.KindFloat {
			return types.KindFloat, nil
		}
		return types.KindNumeric, nil
	case isTemporal(l) && isTemporal(r):
		return widestTemporal(l, r), nil
	default:
		return types.KindNull, errNoCommonType
	}
}

var errNoCommonType = pgerr.New(pgerr.DatatypeMismatch, "no common type")

// setOpSortKeys binds an ORDER BY that follows a set operation.
//
// Only an output column may be named, by position or by name. That is
// PostgreSQL's restriction and it is not arbitrary: the arms are separate
// queries with separate scopes, so a term naming a column of one of them has no
// single meaning -- and picking one arm's would quietly give an answer that
// depends on which side was written first.
func setOpSortKeys(items []ast.OrderByItem, cols []plan.ResultColumn) ([]plan.SortKey, error) {
	keys := make([]plan.SortKey, 0, len(items))
	for _, item := range items {
		ordinal, err := setOpSortTarget(item.Expr, cols)
		if err != nil {
			return nil, err
		}
		desc := item.Dir == ast.SortDesc
		keys = append(keys, plan.SortKey{
			Expr:       &plan.Column{Ordinal: ordinal, Kind: cols[ordinal].Type.Kind, Name: cols[ordinal].Name},
			Desc:       desc,
			NullsFirst: nullsFirst(item.Nulls, desc),
		})
	}
	return keys, nil
}

func setOpSortTarget(e ast.Expr, cols []plan.ResultColumn) (int, error) {
	switch e := e.(type) {
	case *ast.Literal:
		if e.Kind == ast.LitNumber {
			n, err := strconv.Atoi(e.Val)
			if err != nil || n < 1 || n > len(cols) {
				return 0, pgerr.Newf(pgerr.InvalidColumnReference,
					"ORDER BY position %s is not in the select list", e.Val).At(e.Pos())
			}
			return n - 1, nil
		}
	case *ast.ColumnRef:
		if e.Table.IsEmpty() && e.Schema.IsEmpty() {
			for i := range cols {
				if cols[i].Name == e.Column.Name {
					return i, nil
				}
			}
		}
	}
	return 0, pgerr.New(pgerr.InvalidColumnReference,
		"ORDER BY on a set operation must name an output column, by name or position").At(e.Pos())
}
