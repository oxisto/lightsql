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
