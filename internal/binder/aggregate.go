package binder

import (
	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/builtin"
	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/types"
)

// aggContext is what a scope carries while binding the parts of a SELECT that
// sit above grouping: the select list, HAVING and ORDER BY.
//
// Those expressions are not evaluated against an input row. They are evaluated
// against the grouped row, which is the group keys followed by the aggregate
// results. Binding therefore rewrites each one into ordinals over that layout,
// so the executor never has to know which expression an ordinal came from.
type aggContext struct {
	// keys are the GROUP BY expressions, in output order. text is the printed
	// form of the source expression, which is how a select-list term is
	// recognised as naming the same expression.
	keys []aggKey
	// aggs are the aggregate calls found so far, in output order. They are
	// appended to as binding proceeds, and an ordinal handed out early stays
	// valid because keys are fixed before any of this runs.
	aggs []plan.AggCall
}

type aggKey struct {
	text string
	expr plan.Expr
	// name and typ are how the key column is reported. Both are captured while
	// the input scope is still in place, because that is the only point at
	// which a bare column reference can still be resolved back to the type it
	// was declared with.
	name string
	typ  catalog.Type
}

// column returns the reference to a group key matching e, if there is one.
//
// The comparison is on the printed source, which is how PostgreSQL's structural
// match behaves for the cases that matter: GROUP BY a matches a bare a, and
// GROUP BY a + b matches a + b written the same way. It does not claim that
// b + a is the same expression, and neither does PostgreSQL.
func (c *aggContext) column(e ast.Expr) (plan.Expr, bool) {
	text := ast.Sprint(e)
	for i, k := range c.keys {
		if k.text == text {
			return &plan.Column{Ordinal: i, Kind: k.expr.Type(), Name: k.name}, true
		}
	}
	return nil, false
}

// resultType reports how the column at a grouped-row ordinal is declared.
//
// The select list of a grouped query addresses the grouped row — the keys
// followed by the aggregate results — so its ordinals do not index the input
// scope. Looking them up there is not merely imprecise, it reads an unrelated
// column: count(*) at grouped ordinal 1 took the declared type of whichever
// input column happened to sit at ordinal 1, and reported an integer count as
// text. Nothing consumed that type until a derived table did, at which point
// it became a comparison against the wrong type rather than a bad label.
func (c *aggContext) resultType(ord int) (catalog.Type, bool) {
	switch {
	case ord < 0:
		return catalog.Type{}, false
	case ord < len(c.keys):
		return c.keys[ord].typ, true
	}
	if i := ord - len(c.keys); i < len(c.aggs) {
		k := c.aggs[i].Kind
		return catalog.Type{Kind: k, Name: k.String()}, true
	}
	return catalog.Type{}, false
}

// add registers an aggregate call and returns the reference to its result.
func (c *aggContext) add(fc *ast.FuncCall, agg *builtin.Aggregate, sc *scope) (plan.Expr, error) {
	call := plan.AggCall{Func: agg.Name, Distinct: fc.Distinct}

	switch {
	case fc.Star:
		if agg.Name != "count" {
			return nil, pgerr.Newf(pgerr.SyntaxError,
				"%s(*) is not a valid aggregate call", agg.Name).At(fc.Pos())
		}
		if fc.Distinct {
			return nil, pgerr.New(pgerr.SyntaxError,
				"count(DISTINCT *) is not valid").At(fc.Pos())
		}
		call.Kind = types.KindInt

	case len(fc.Args) != 1:
		return nil, pgerr.Newf(pgerr.SyntaxError,
			"%s takes exactly one argument", agg.Name).At(fc.Pos())

	default:
		// The argument sees the input row, not the grouped one — inside
		// count(x), x is a column of the table being grouped. Binding it in a
		// scope without the aggregate context both gives it that meaning and
		// makes a nested aggregate fall through to the "not allowed here"
		// error, since count(sum(x)) has no sensible reading.
		inner := *sc
		inner.agg = nil
		arg, err := bindExpr(fc.Args[0], &inner)
		if err != nil {
			return nil, err
		}
		// sum and avg read the numeric payload of a value. Value keeps that
		// payload in the same field whatever the kind, so sum(text) would not
		// fail at runtime — it would return 0, and sum(boolean) would return
		// the number of true rows. Refusing here is what keeps a wrong answer
		// from looking like a right one.
		if agg.Numeric && !arg.Type().IsNumeric() && arg.Type() != types.KindNull {
			return nil, pgerr.Newf(pgerr.UndefinedFunction,
				"function %s(%s) does not exist", agg.Name, arg.Type()).At(fc.Pos())
		}
		call.Arg = arg
		call.Kind = agg.Result(arg.Type())
	}

	c.aggs = append(c.aggs, call)
	return &plan.Column{
		Ordinal: len(c.keys) + len(c.aggs) - 1,
		Kind:    call.Kind,
		Name:    agg.Name,
	}, nil
}

// bindFuncCall resolves a function call.
//
// Only aggregates exist so far, and they are legal only where a grouped row is
// in scope. Calling one in WHERE is the common mistake, and it has its own
// message because "function does not exist" would send the reader looking for
// the wrong problem: WHERE runs before grouping, so the aggregate has nothing
// to aggregate over yet.
func bindFuncCall(fc *ast.FuncCall, sc *scope) (plan.Expr, error) {
	// A scalar function is legal anywhere an expression is, including inside an
	// aggregate and below a grouping, so it is resolved before the aggregate
	// rules below get a say.
	if e, ok, err := bindScalarCall(fc, sc); ok {
		return e, err
	}

	agg, ok := builtin.LookupAggregate(fc.Name.Name)
	if !ok {
		return nil, pgerr.Newf(pgerr.UndefinedFunction,
			"function %s does not exist", fc.Name.Name).At(fc.Pos())
	}
	if sc.agg == nil {
		where := sc.clause
		if where == "" {
			where = "this context"
		}
		return nil, pgerr.Newf(pgerr.GroupingError,
			"aggregate functions are not allowed in %s", where).At(fc.Pos())
	}
	return sc.agg.add(fc, agg, sc)
}

// hasAggregate reports whether an expression contains an aggregate call, which
// is what decides that a SELECT without GROUP BY is nonetheless a grouped
// query: SELECT count(*) FROM t has one group covering every row.
func hasAggregate(e ast.Expr) bool {
	found := false
	walkExpr(e, func(n ast.Expr) {
		if fc, ok := n.(*ast.FuncCall); ok && builtin.IsAggregate(fc.Name.Name) {
			found = true
		}
	})
	return found
}

// walkExpr calls fn for e and every expression beneath it.
//
// The switch is exhaustive over the expression nodes rather than reflective,
// so adding a node type that can contain an aggregate is a compile-time
// prompt to handle it here.
func walkExpr(e ast.Expr, fn func(ast.Expr)) {
	if e == nil {
		return
	}
	fn(e)

	switch e := e.(type) {
	case *ast.ParenExpr:
		walkExpr(e.X, fn)
	case *ast.UnaryExpr:
		walkExpr(e.X, fn)
	case *ast.BinaryExpr:
		walkExpr(e.X, fn)
		walkExpr(e.Y, fn)
	case *ast.IsNullExpr:
		walkExpr(e.X, fn)
	case *ast.CastExpr:
		walkExpr(e.X, fn)
	case *ast.FuncCall:
		for _, a := range e.Args {
			walkExpr(a, fn)
		}
	case *ast.InExpr:
		walkExpr(e.X, fn)
		for _, a := range e.List {
			walkExpr(a, fn)
		}
	case *ast.BetweenExpr:
		walkExpr(e.X, fn)
		walkExpr(e.Lo, fn)
		walkExpr(e.Hi, fn)
	case *ast.CaseExpr:
		walkExpr(e.Operand, fn)
		for _, w := range e.Whens {
			walkExpr(w.Cond, fn)
			walkExpr(w.Value, fn)
		}
		walkExpr(e.Else, fn)
	}
}

// selectHasAggregate reports whether the select list or ORDER BY mentions an
// aggregate, which makes the query grouped even without a GROUP BY clause.
func selectHasAggregate(s *ast.SelectStmt) bool {
	for _, item := range s.Items {
		if hasAggregate(item.Expr) {
			return true
		}
	}
	for _, item := range s.OrderBy {
		if hasAggregate(item.Expr) {
			return true
		}
	}
	return false
}

// beginGrouping binds the GROUP BY expressions and puts the scope into
// aggregate mode, so everything bound afterwards is resolved against the
// grouped row.
func (b *Binder) beginGrouping(s *ast.SelectStmt, sc *scope) error {
	ctx := &aggContext{}
	for _, g := range s.GroupBy {
		// A key is bound against the input, since that is what it is computed
		// from. Binding it before the scope enters aggregate mode is what stops
		// it from trying to resolve against the grouped row that does not exist
		// yet.
		expr, err := bindExpr(g, sc)
		if err != nil {
			return err
		}
		ctx.keys = append(ctx.keys, aggKey{
			text: ast.Sprint(g),
			expr: expr,
			name: keyName(g, expr),
			// Captured here, not in groupingNode, because the scope enters
			// aggregate mode as soon as this loop ends.
			typ: resultType(expr, sc),
		})
	}
	sc.agg = ctx
	return nil
}

// keyName is the label a GROUP BY key is reported under: the column's own name
// for a bare reference, and PostgreSQL's fallback otherwise. It mirrors
// outputName, which does the same job for a select item.
func keyName(src ast.Expr, bound plan.Expr) string {
	if ref, ok := src.(*ast.ColumnRef); ok {
		return ref.Column.Name
	}
	if c, ok := bound.(*plan.Column); ok && c.Name != "" {
		return c.Name
	}
	return "?column?"
}

// groupingNode builds the Aggregate from what binding collected.
func (b *Binder) groupingNode(input plan.Node, sc *scope) plan.Node {
	ctx := sc.agg
	agg := &plan.Aggregate{Input: input}

	for _, k := range ctx.keys {
		agg.Keys = append(agg.Keys, k.expr)
		agg.Cols = append(agg.Cols, plan.ResultColumn{Name: k.name, Type: k.typ})
	}
	for _, c := range ctx.Aggs() {
		agg.Aggs = append(agg.Aggs, c)
		agg.Cols = append(agg.Cols, plan.ResultColumn{
			Name: c.Func, Type: catalog.Type{Kind: c.Kind, Name: c.Kind.String()},
		})
	}
	return agg
}

// Aggs reports the calls collected during binding.
func (c *aggContext) Aggs() []plan.AggCall { return c.aggs }
