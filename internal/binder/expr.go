package binder

import (
	"strconv"

	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/token"
	"github.com/oxisto/lightsql/internal/types"
)

// bindExpr resolves an expression against a scope, producing a typed plan
// expression whose result kind is known statically.
func bindExpr(e ast.Expr, sc *scope) (plan.Expr, error) {
	// Above a grouping, an expression is evaluated over the grouped row. A term
	// that names a GROUP BY expression becomes a reference to that key, whole,
	// before its parts are looked at — which is what makes GROUP BY a + b work
	// when a and b are not themselves grouped.
	if sc.agg != nil {
		if col, ok := sc.agg.column(e); ok {
			return col, nil
		}
	}

	switch e := e.(type) {
	case *ast.ParenExpr:
		// Grouping has done its job in the parser; it carries no meaning here.
		return bindExpr(e.X, sc)

	case *ast.Literal:
		return bindLiteral(e)

	case *ast.Param:
		// A parameter's type is not known from the text. It is inferred from
		// the context that consumes it — coerce fills this in — and defaults to
		// text when nothing constrains it.
		return &plan.Param{Ord: e.Ord, Kind: types.KindNull}, nil

	case *ast.ColumnRef:
		c, err := sc.resolve(e)
		if err != nil {
			return nil, err
		}
		// Reaching here in a grouped query means the column is neither a group
		// key nor inside an aggregate, so there is no single value to report
		// for the group. The check sits after resolve so that a misspelled name
		// is still reported as a missing column rather than as this.
		if sc.agg != nil {
			return nil, pgerr.Newf(pgerr.GroupingError,
				"column %q must appear in the GROUP BY clause or be used in an aggregate function",
				e.String()).At(e.Pos())
		}
		return &plan.Column{Ordinal: c.ordinal, Kind: c.typ.Kind, Name: c.name}, nil

	case *ast.IsNullExpr:
		x, err := bindExpr(e.X, sc)
		if err != nil {
			return nil, err
		}
		return &plan.IsNull{X: x, Negate: e.Negate}, nil

	case *ast.CastExpr:
		return bindCast(e, sc)

	case *ast.UnaryExpr:
		return bindUnary(e, sc)

	case *ast.BinaryExpr:
		return bindBinary(e, sc)

	case *ast.SubqueryExpr:
		return bindScalarSubquery(e, sc)

	case *ast.ExistsExpr:
		return bindExists(e, sc)

	case *ast.InExpr:
		return bindIn(e, sc)

	case *ast.FuncCall:
		return bindFuncCall(e, sc)

	case *ast.Star:
		return nil, pgerr.New(pgerr.SyntaxError,
			"* is not valid in this context").At(e.Pos())

	default:
		return nil, pgerr.Newf(pgerr.FeatureNotSupported,
			"expression %T is not supported yet", e).At(e.Pos())
	}
}

// bindLiteral converts a literal's source text into a typed value.
//
// A numeric literal becomes an integer when it fits one and a float otherwise,
// which is what makes `WHERE id = 1` compare as integers without a cast.
// subplan binds a SELECT that appears inside an expression.
//
// The body is bound in a scope of its own, so it cannot see the row the outer
// query is processing. Correlated subqueries are not supported yet, and this is
// what makes one fail with an unresolved column rather than resolve against an
// ordinal belonging to a row the subplan never sees.
func subplan(sel *ast.SelectStmt, sc *scope, pos token.Pos) (plan.Node, error) {
	if sc.b == nil {
		// The scopes without a binder are the ones where SQL forbids a
		// subquery: a CHECK constraint and a DEFAULT expression, neither of
		// which has a row to run one against.
		return nil, pgerr.New(pgerr.FeatureNotSupported,
			"cannot use a subquery in this context").At(pos)
	}
	return sc.b.bindSelectNode(sel)
}

// oneColumn rejects a subquery used where a value is expected but which
// produces a whole row instead.
func oneColumn(n plan.Node, pos token.Pos) error {
	if len(n.Result()) != 1 {
		return pgerr.New(pgerr.SyntaxError,
			"subquery must return only one column").At(pos)
	}
	return nil
}

func bindScalarSubquery(e *ast.SubqueryExpr, sc *scope) (plan.Expr, error) {
	node, err := subplan(e.Select, sc, e.Pos())
	if err != nil {
		return nil, err
	}
	if err := oneColumn(node, e.Pos()); err != nil {
		return nil, err
	}
	return &plan.ScalarSubquery{Input: node, Kind: node.Result()[0].Type.Kind}, nil
}

func bindExists(e *ast.ExistsExpr, sc *scope) (plan.Expr, error) {
	node, err := subplan(e.Subquery, sc, e.Pos())
	if err != nil {
		return nil, err
	}
	// EXISTS asks only whether a row is there, so its select list is not
	// constrained: SELECT *, SELECT 1 and SELECT a, b all mean the same to it.
	return &plan.ExistsSubquery{Input: node, Negate: e.Negate}, nil
}

func bindIn(e *ast.InExpr, sc *scope) (plan.Expr, error) {
	x, err := bindExpr(e.X, sc)
	if err != nil {
		return nil, err
	}

	if e.Subquery != nil {
		node, err := subplan(e.Subquery, sc, e.Pos())
		if err != nil {
			return nil, err
		}
		if err := oneColumn(node, e.Pos()); err != nil {
			return nil, err
		}
		if x, err = coerce(x, node.Result()[0].Type.Kind, e.X.Pos()); err != nil {
			return nil, err
		}
		return &plan.InSubquery{X: x, Input: node, Negate: e.Negate}, nil
	}

	// The list is checked against the left operand's type here rather than at
	// runtime, so that `n IN ('a')` is a type error like `n = 'a'` is, instead
	// of a comparison that quietly matches nothing.
	list := make([]plan.Expr, 0, len(e.List))
	for _, item := range e.List {
		v, err := bindExpr(item, sc)
		if err != nil {
			return nil, err
		}
		// An unresolved parameter on the left takes its type from the first
		// element that has one, mirroring how unify treats a comparison.
		if p, ok := x.(*plan.Param); ok && p.Kind == types.KindNull {
			p.Kind = v.Type()
		}
		if v, err = coerce(v, x.Type(), item.Pos()); err != nil {
			return nil, err
		}
		list = append(list, v)
	}
	return &plan.InList{X: x, List: list, Negate: e.Negate}, nil
}

func bindLiteral(e *ast.Literal) (plan.Expr, error) {
	switch e.Kind {
	case ast.LitNull:
		return &plan.Const{Val: types.Null()}, nil
	case ast.LitTrue:
		return &plan.Const{Val: types.Bool(true)}, nil
	case ast.LitFalse:
		return &plan.Const{Val: types.Bool(false)}, nil
	case ast.LitString:
		return &plan.Const{Val: types.Text(e.Val)}, nil
	}

	if i, err := strconv.ParseInt(e.Val, 10, 64); err == nil {
		return &plan.Const{Val: types.Int(i)}, nil
	}
	f, err := strconv.ParseFloat(e.Val, 64)
	if err != nil {
		return nil, pgerr.Newf(pgerr.InvalidTextForType,
			"invalid numeric literal %q", e.Val).At(e.Pos())
	}
	return &plan.Const{Val: types.Float(f)}, nil
}

// bindCast resolves CAST(x AS t) and its x::t spelling.
//
// Unlike coerce, which applies only the conversions PostgreSQL performs
// implicitly, an explicit cast is the user asking for the conversion, so it is
// attempted even where an implicit one would be refused.
func bindCast(e *ast.CastExpr, sc *scope) (plan.Expr, error) {
	x, err := bindExpr(e.X, sc)
	if err != nil {
		return nil, err
	}
	t, err := catalog.ResolveType(e.Type.Name, e.Type.Mods)
	if err != nil {
		return nil, pgerr.New(pgerr.UndefinedObject, err.Error()).At(e.Type.Pos())
	}

	if x.Type() == t.Kind {
		return x, nil
	}
	// A parameter takes the cast type outright: $1::jsonb tells the executor
	// what to convert the argument to, which is the whole point of writing it.
	if p, ok := x.(*plan.Param); ok {
		p.Kind = t.Kind
		return p, nil
	}
	if c, ok := x.(*plan.Const); ok {
		v, err := types.Cast(c.Val, t.Kind)
		if err != nil {
			return nil, pgerr.New(pgerr.InvalidTextForType, err.Error()).At(e.X.Pos())
		}
		return &plan.Const{Val: v}, nil
	}
	return &plan.Cast{X: x, Kind: t.Kind}, nil
}

func bindUnary(e *ast.UnaryExpr, sc *scope) (plan.Expr, error) {
	x, err := bindExpr(e.X, sc)
	if err != nil {
		return nil, err
	}

	switch e.Op {
	case ast.OpNot:
		if x, err = coerce(x, types.KindBool, e.X.Pos()); err != nil {
			return nil, err
		}
		return &plan.Unary{Op: e.Op, X: x, Kind: types.KindBool}, nil

	default: // OpNeg, OpPlus
		if !x.Type().IsNumeric() && x.Type() != types.KindNull {
			return nil, pgerr.Newf(pgerr.DatatypeMismatch,
				"operator %s is not defined for type %s", e.Op, x.Type()).At(e.Pos())
		}
		return &plan.Unary{Op: e.Op, X: x, Kind: x.Type()}, nil
	}
}

func bindBinary(e *ast.BinaryExpr, sc *scope) (plan.Expr, error) {
	l, err := bindExpr(e.X, sc)
	if err != nil {
		return nil, err
	}
	r, err := bindExpr(e.Y, sc)
	if err != nil {
		return nil, err
	}

	switch {
	case e.Op == ast.OpAnd || e.Op == ast.OpOr:
		if l, err = coerce(l, types.KindBool, e.X.Pos()); err != nil {
			return nil, err
		}
		if r, err = coerce(r, types.KindBool, e.Y.Pos()); err != nil {
			return nil, err
		}
		return &plan.Binary{Op: e.Op, L: l, R: r, Kind: types.KindBool}, nil

	case e.Op == ast.OpConcat:
		if l, err = coerce(l, types.KindText, e.X.Pos()); err != nil {
			return nil, err
		}
		if r, err = coerce(r, types.KindText, e.Y.Pos()); err != nil {
			return nil, err
		}
		return &plan.Binary{Op: e.Op, L: l, R: r, Kind: types.KindText}, nil

	case e.Op.IsComparison(), e.Op == ast.OpIsDistinctFrom, e.Op == ast.OpIsNotDistinctFrom:
		if l, r, err = unify(l, r, e); err != nil {
			return nil, err
		}
		return &plan.Binary{Op: e.Op, L: l, R: r, Kind: types.KindBool}, nil

	case e.Op == ast.OpJSONField, e.Op == ast.OpJSONText:
		if !isJSON(l.Type()) && l.Type() != types.KindNull {
			return nil, pgerr.Newf(pgerr.DatatypeMismatch,
				"operator %s requires json or jsonb on the left, not %s", e.Op, l.Type()).At(e.OpPos)
		}
		// The right side selects either an object member by name or an array
		// element by position, so both text and integer are legal and the
		// choice is made per row rather than at bind time.
		if r.Type() != types.KindText && r.Type() != types.KindInt && r.Type() != types.KindNull {
			return nil, pgerr.Newf(pgerr.DatatypeMismatch,
				"operator %s requires text or integer on the right, not %s", e.Op, r.Type()).At(e.OpPos)
		}
		// -> keeps the document's own kind so that chained access stays JSON;
		// ->> unwraps to text, which is the whole difference between them.
		kind := l.Type()
		if e.Op == ast.OpJSONText {
			kind = types.KindText
		}
		return &plan.Binary{Op: e.Op, L: l, R: r, Kind: kind}, nil

	case e.Op == ast.OpJSONContains:
		if !isJSON(l.Type()) && l.Type() != types.KindNull {
			return nil, pgerr.Newf(pgerr.DatatypeMismatch,
				"operator %s requires json or jsonb, not %s", e.Op, l.Type()).At(e.OpPos)
		}
		if r, err = coerce(r, l.Type(), e.Y.Pos()); err != nil {
			return nil, err
		}
		return &plan.Binary{Op: e.Op, L: l, R: r, Kind: types.KindBool}, nil

	case e.Op == ast.OpLike || e.Op == ast.OpNotLike:
		if l, err = coerce(l, types.KindText, e.X.Pos()); err != nil {
			return nil, err
		}
		if r, err = coerce(r, types.KindText, e.Y.Pos()); err != nil {
			return nil, err
		}
		return &plan.Binary{Op: e.Op, L: l, R: r, Kind: types.KindBool}, nil

	default: // arithmetic
		if l, r, err = unify(l, r, e); err != nil {
			return nil, err
		}
		kind := l.Type()
		if kind == types.KindNull {
			kind = r.Type()
		}
		if !kind.IsNumeric() && kind != types.KindNull {
			return nil, pgerr.Newf(pgerr.DatatypeMismatch,
				"operator %s is not defined for type %s", e.Op, kind).At(e.OpPos)
		}
		return &plan.Binary{Op: e.Op, L: l, R: r, Kind: kind}, nil
	}
}

func isJSON(k types.Kind) bool { return k == types.KindJSON || k == types.KindJSONB }

// unify brings two operands to a common type so the executor can compare or
// combine them without inspecting types per row.
//
// An untyped parameter takes the other side's type, a NULL literal is compatible
// with anything, and an integer promotes to a float when the other side is one.
// Anything else is a type error reported here rather than producing a surprising
// answer at runtime.
func unify(l, r plan.Expr, e *ast.BinaryExpr) (left, right plan.Expr, err error) {
	lt, rt := l.Type(), r.Type()

	if p, ok := l.(*plan.Param); ok && lt == types.KindNull && rt != types.KindNull {
		p.Kind = rt
		return l, r, nil
	}
	if p, ok := r.(*plan.Param); ok && rt == types.KindNull && lt != types.KindNull {
		p.Kind = lt
		return l, r, nil
	}
	if lt == rt || lt == types.KindNull || rt == types.KindNull {
		return l, r, nil
	}
	if lt.IsNumeric() && rt.IsNumeric() {
		// Comparison already promotes int against float, so no cast node is
		// needed; the value model handles it.
		return l, r, nil
	}

	// A string literal compared against a document resolves to that document
	// type. PostgreSQL gives a quoted literal the "unknown" type and lets the
	// other operand decide; lightsql commits it to text in the binder, so
	// without this doc = '{"a":1}' would be a type error and a string literal —
	// the only way to write a document — could not be compared at all. The rule
	// is kept to json and jsonb rather than applied to every pair, because
	// resolving text against, say, an integer would silently accept comparisons
	// PostgreSQL rejects.
	if c, ok := l.(*plan.Const); ok && lt == types.KindText && isJSON(rt) {
		v, err := types.Cast(c.Val, rt)
		if err != nil {
			return nil, nil, pgerr.New(pgerr.InvalidTextForType, err.Error()).At(e.X.Pos())
		}
		return &plan.Const{Val: v}, r, nil
	}
	if c, ok := r.(*plan.Const); ok && rt == types.KindText && isJSON(lt) {
		v, err := types.Cast(c.Val, lt)
		if err != nil {
			return nil, nil, pgerr.New(pgerr.InvalidTextForType, err.Error()).At(e.Y.Pos())
		}
		return l, &plan.Const{Val: v}, nil
	}

	return nil, nil, pgerr.Newf(pgerr.DatatypeMismatch,
		"operator %s cannot be applied to types %s and %s", e.Op, lt, rt).At(e.OpPos)
}

// coerce adapts an expression to a required type, or reports why it cannot.
//
// Only conversions PostgreSQL performs implicitly are done here. A literal is
// converted at bind time so the cost is not paid per row; a parameter simply
// records the type it will be given.
func coerce(e plan.Expr, want types.Kind, pos token.Pos) (plan.Expr, error) {
	// A parameter is tested before the checks below, because an unresolved one
	// reports KindNull and would otherwise be waved through as "compatible with
	// anything" without ever recording the type it must be converted to. That
	// left the executor with nothing to convert against, so an INSERT stored a
	// caller's string verbatim into an integer column.
	if p, ok := e.(*plan.Param); ok {
		if p.Kind == types.KindNull {
			p.Kind = want
		}
		return p, nil
	}

	got := e.Type()
	if got == want || got == types.KindNull {
		return e, nil
	}

	// A constant is converted now, so the cost is paid once per statement rather
	// than once per row, and an impossible conversion is reported before any
	// rows have been returned.
	if c, ok := e.(*plan.Const); ok {
		v, err := types.Cast(c.Val, want)
		if err != nil {
			return nil, pgerr.New(pgerr.InvalidTextForType, err.Error()).At(pos)
		}
		return &plan.Const{Val: v}, nil
	}

	if got.IsNumeric() && want.IsNumeric() {
		return e, nil
	}
	return nil, pgerr.Newf(pgerr.DatatypeMismatch,
		"expression of type %s cannot be used where %s is expected", got, want).At(pos)
}
