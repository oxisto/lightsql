// Package exec runs a bound plan.
//
// Two choices shape this package. Expressions are compiled once into closures
// rather than walked as a tree per row, so the type dispatch happens at plan
// time and the per-row cost is a call. And operators are streaming iterators, so
// a LIMIT can stop early and an intermediate result never has to be materialised
// in full.
package exec

import (
	"math"

	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/types"
)

// Row is one tuple, addressed by column ordinal.
type Row []types.Value

// Eval is a compiled expression. Compiling to a closure moves the decision about
// which operation to perform out of the per-row path: by the time Eval is
// called, the operator and operand types have already been chosen.
type Eval func(args []types.Value, row Row) (types.Value, error)

// Compile turns a bound expression into a closure.
func Compile(e plan.Expr) (Eval, error) {
	switch e := e.(type) {
	case *plan.Const:
		v := e.Val
		return func([]types.Value, Row) (types.Value, error) { return v, nil }, nil

	case *plan.Column:
		i := e.Ordinal
		return func(_ []types.Value, row Row) (types.Value, error) { return row[i], nil }, nil

	case *plan.Param:
		i := e.Ord - 1
		// The binder inferred what this placeholder must be from the context
		// that consumes it, but the caller supplies whatever Go type they had.
		// Converting here is what makes db.Query("... WHERE id = $1", "7")
		// behave the same as passing the integer 7, rather than silently
		// matching nothing because a text value never equals an integer.
		want := e.Kind
		return func(args []types.Value, _ Row) (types.Value, error) {
			if i < 0 || i >= len(args) {
				return types.Value{}, pgerr.Newf(pgerr.SyntaxError,
					"there is no parameter $%d", i+1)
			}
			v := args[i]
			if want == types.KindNull || v.Kind() == want {
				return v, nil
			}
			out, err := types.Cast(v, want)
			if err != nil {
				return types.Value{}, pgerr.New(pgerr.InvalidTextForType, err.Error())
			}
			return out, nil
		}, nil

	case *plan.IsNull:
		x, err := Compile(e.X)
		if err != nil {
			return nil, err
		}
		negate := e.Negate
		return func(args []types.Value, row Row) (types.Value, error) {
			v, err := x(args, row)
			if err != nil {
				return types.Value{}, err
			}
			// IS NULL is always definite, never unknown.
			return types.Bool(v.IsNull() != negate), nil
		}, nil

	case *plan.Unary:
		return compileUnary(e)

	case *plan.Binary:
		return compileBinary(e)

	default:
		return nil, pgerr.Newf(pgerr.InternalError, "cannot compile expression %T", e)
	}
}

func compileUnary(e *plan.Unary) (Eval, error) {
	x, err := Compile(e.X)
	if err != nil {
		return nil, err
	}

	switch e.Op {
	case ast.OpPlus:
		return x, nil

	case ast.OpNot:
		return func(args []types.Value, row Row) (types.Value, error) {
			v, err := x(args, row)
			if err != nil {
				return types.Value{}, err
			}
			// NOT UNKNOWN is UNKNOWN, which Bool3 handles and a Go bool
			// would silently turn into true.
			return v.Truth().Not().Value(), nil
		}, nil

	default: // OpNeg
		return func(args []types.Value, row Row) (types.Value, error) {
			v, err := x(args, row)
			if err != nil {
				return types.Value{}, err
			}
			switch v.Kind() {
			case types.KindNull:
				return v, nil
			case types.KindInt:
				return types.Int(-v.AsInt()), nil
			default:
				return types.Float(-v.AsFloat()), nil
			}
		}, nil
	}
}

// comparisons maps each comparison operator to its three-valued implementation.
// Routing every comparison through types keeps the NULL rules in one place
// instead of repeating them per operator.
var comparisons = map[ast.BinaryOp]func(a, b types.Value) types.Bool3{
	ast.OpEq:             types.Eq,
	ast.OpNe:             types.Ne,
	ast.OpLt:             types.Lt,
	ast.OpLe:             types.Le,
	ast.OpGt:             types.Gt,
	ast.OpGe:             types.Ge,
	ast.OpIsDistinctFrom: types.IsDistinctFrom,
	ast.OpIsNotDistinctFrom: func(a, b types.Value) types.Bool3 {
		return types.IsDistinctFrom(a, b).Not()
	},
}

func compileBinary(e *plan.Binary) (Eval, error) {
	l, err := Compile(e.L)
	if err != nil {
		return nil, err
	}
	r, err := Compile(e.R)
	if err != nil {
		return nil, err
	}

	// AND and OR short-circuit, so they cannot use the generic path that
	// evaluates both operands first.
	switch e.Op {
	case ast.OpAnd:
		return func(args []types.Value, row Row) (types.Value, error) {
			lv, err := l(args, row)
			if err != nil {
				return types.Value{}, err
			}
			// FALSE dominates AND, so the right operand need not be evaluated.
			if lv.Truth() == types.False {
				return types.Bool(false), nil
			}
			rv, err := r(args, row)
			if err != nil {
				return types.Value{}, err
			}
			return lv.Truth().And(rv.Truth()).Value(), nil
		}, nil

	case ast.OpOr:
		return func(args []types.Value, row Row) (types.Value, error) {
			lv, err := l(args, row)
			if err != nil {
				return types.Value{}, err
			}
			if lv.Truth() == types.True {
				return types.Bool(true), nil
			}
			rv, err := r(args, row)
			if err != nil {
				return types.Value{}, err
			}
			return lv.Truth().Or(rv.Truth()).Value(), nil
		}, nil
	}

	if cmp, ok := comparisons[e.Op]; ok {
		return func(args []types.Value, row Row) (types.Value, error) {
			lv, rv, err := evalPair(l, r, args, row)
			if err != nil {
				return types.Value{}, err
			}
			return cmp(lv, rv).Value(), nil
		}, nil
	}

	if e.Op == ast.OpConcat {
		return func(args []types.Value, row Row) (types.Value, error) {
			lv, rv, err := evalPair(l, r, args, row)
			if err != nil {
				return types.Value{}, err
			}
			// Concatenating with NULL yields NULL, as in PostgreSQL.
			if lv.IsNull() || rv.IsNull() {
				return types.Null(), nil
			}
			return types.Text(lv.AsString() + rv.AsString()), nil
		}, nil
	}

	return compileArithmetic(e, l, r)
}

func compileArithmetic(e *plan.Binary, l, r Eval) (Eval, error) {
	op := e.Op
	// Integer arithmetic stays exact; a float operand makes the whole
	// expression float, which is what the binder already decided.
	useInt := e.Kind == types.KindInt

	return func(args []types.Value, row Row) (types.Value, error) {
		lv, rv, err := evalPair(l, r, args, row)
		if err != nil {
			return types.Value{}, err
		}
		if lv.IsNull() || rv.IsNull() {
			return types.Null(), nil
		}
		if useInt && lv.Kind() == types.KindInt && rv.Kind() == types.KindInt {
			return intArith(op, lv.AsInt(), rv.AsInt())
		}
		return floatArith(op, lv.AsFloat(), rv.AsFloat())
	}, nil
}

func intArith(op ast.BinaryOp, a, b int64) (types.Value, error) {
	switch op {
	case ast.OpAdd:
		return types.Int(a + b), nil
	case ast.OpSub:
		return types.Int(a - b), nil
	case ast.OpMul:
		return types.Int(a * b), nil
	case ast.OpDiv:
		if b == 0 {
			return types.Value{}, pgerr.New(pgerr.DivisionByZero, "division by zero")
		}
		return types.Int(a / b), nil
	case ast.OpMod:
		if b == 0 {
			return types.Value{}, pgerr.New(pgerr.DivisionByZero, "division by zero")
		}
		return types.Int(a % b), nil
	default: // OpExp
		return types.Float(math.Pow(float64(a), float64(b))), nil
	}
}

func floatArith(op ast.BinaryOp, a, b float64) (types.Value, error) {
	switch op {
	case ast.OpAdd:
		return types.Float(a + b), nil
	case ast.OpSub:
		return types.Float(a - b), nil
	case ast.OpMul:
		return types.Float(a * b), nil
	case ast.OpDiv:
		if b == 0 {
			return types.Value{}, pgerr.New(pgerr.DivisionByZero, "division by zero")
		}
		return types.Float(a / b), nil
	case ast.OpMod:
		if b == 0 {
			return types.Value{}, pgerr.New(pgerr.DivisionByZero, "division by zero")
		}
		return types.Float(math.Mod(a, b)), nil
	default: // OpExp
		return types.Float(math.Pow(a, b)), nil
	}
}

// evalPair evaluates both operands, which every non-short-circuiting operator
// needs.
func evalPair(l, r Eval, args []types.Value, row Row) (types.Value, types.Value, error) {
	lv, err := l(args, row)
	if err != nil {
		return types.Value{}, types.Value{}, err
	}
	rv, err := r(args, row)
	if err != nil {
		return types.Value{}, types.Value{}, err
	}
	return lv, rv, nil
}
