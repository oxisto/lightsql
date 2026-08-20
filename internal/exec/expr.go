// Package exec runs a bound plan.
//
// Two choices shape this package. Expressions are compiled once into closures
// rather than walked as a tree per row, so the type dispatch happens at plan
// time and the per-row cost is a call. And operators are streaming iterators, so
// a LIMIT can stop early and an intermediate result never has to be materialised
// in full.
package exec

import (
	"context"
	"errors"
	"math"

	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/storage"
	"github.com/oxisto/lightsql/internal/types"
)

// Row is one tuple, addressed by column ordinal.
type Row []types.Value

// Eval is a compiled expression. Compiling to a closure moves the decision about
// which operation to perform out of the per-row path: by the time Eval is
// called, the operator and operand types have already been chosen.
//
// The context is carried because an expression is not always cheap. A subquery
// in expression position runs a whole operator tree, and one evaluated per row
// of its outer query must be cancellable like any other work — checking only at
// statement entry would leave a query that cannot be interrupted. Most
// implementations ignore it, which is the point: the cost is a parameter, and
// the alternative is that the one case which needs it cannot have it.
type Eval func(ctx context.Context, args []types.Value, row Row) (types.Value, error)

// Compile turns a bound expression into a closure.
func Compile(e plan.Expr, tx *storage.Tx) (Eval, error) {
	switch e := e.(type) {
	case *plan.Const:
		v := e.Val
		return func(context.Context, []types.Value, Row) (types.Value, error) { return v, nil }, nil

	case *plan.Column:
		i := e.Ordinal
		return func(_ context.Context, _ []types.Value, row Row) (types.Value, error) { return row[i], nil }, nil

	case *plan.Param:
		i := e.Ord - 1
		// The binder inferred what this placeholder must be from the context
		// that consumes it, but the caller supplies whatever Go type they had.
		// Converting here is what makes db.Query("... WHERE id = $1", "7")
		// behave the same as passing the integer 7, rather than silently
		// matching nothing because a text value never equals an integer.
		want := e.Kind
		return func(_ context.Context, args []types.Value, _ Row) (types.Value, error) {
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
		x, err := Compile(e.X, tx)
		if err != nil {
			return nil, err
		}
		negate := e.Negate
		return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
			v, err := x(ctx, args, row)
			if err != nil {
				return types.Value{}, err
			}
			// IS NULL is always definite, never unknown.
			return types.Bool(v.IsNull() != negate), nil
		}, nil

	case *plan.Cast:
		x, err := Compile(e.X, tx)
		if err != nil {
			return nil, err
		}
		want := e.Kind
		return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
			v, err := x(ctx, args, row)
			if err != nil {
				return types.Value{}, err
			}
			out, err := types.Cast(v, want)
			if err != nil {
				return types.Value{}, pgerr.New(pgerr.InvalidTextForType, err.Error())
			}
			return out, nil
		}, nil

	case *plan.Case:
		return compileCase(e, tx)

	case *plan.ScalarSubquery:
		return compileScalarSubquery(e, tx)

	case *plan.ExistsSubquery:
		return compileExists(e, tx)

	case *plan.InSubquery:
		return compileInSubquery(e, tx)

	case *plan.InList:
		return compileInList(e, tx)

	case *plan.Unary:
		return compileUnary(e, tx)

	case *plan.Binary:
		return compileBinary(e, tx)

	default:
		return nil, pgerr.Newf(pgerr.InternalError, "cannot compile expression %T", e)
	}
}

// compileCase runs the arms in order and stops at the first whose condition is
// true.
//
// Only true fires an arm: false and unknown are both skipped, which is the same
// rule a WHERE clause follows and the reason `CASE WHEN NULL THEN 1 ELSE 2 END`
// is 2 rather than 1. A CASE that matches nothing with no ELSE is NULL, which
// is why the zero Value is never returned from here.
func compileCase(e *plan.Case, tx *storage.Tx) (Eval, error) {
	type arm struct{ cond, value Eval }

	arms := make([]arm, len(e.Whens))
	for i, w := range e.Whens {
		cond, err := Compile(w.Cond, tx)
		if err != nil {
			return nil, err
		}
		value, err := Compile(w.Value, tx)
		if err != nil {
			return nil, err
		}
		arms[i] = arm{cond: cond, value: value}
	}

	var els Eval
	if e.Else != nil {
		var err error
		if els, err = Compile(e.Else, tx); err != nil {
			return nil, err
		}
	}

	return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
		for _, a := range arms {
			v, err := a.cond(ctx, args, row)
			if err != nil {
				return types.Value{}, err
			}
			if v.Truth() != types.True {
				continue
			}
			// Only the arm that fires is evaluated, so a later branch that
			// would divide by zero costs nothing when an earlier one matches.
			return a.value(ctx, args, row)
		}
		if els != nil {
			return els(ctx, args, row)
		}
		return types.Null(), nil
	}, nil
}

// Subqueries in expression position are uncorrelated: the binder gives one a
// scope of its own, so it cannot read the outer row and its answer is therefore
// the same for every row. Each of the closures below runs its subplan once and
// keeps the result.
//
// That cache lives in the closure, which is safe because a closure is created
// per execution -- Compile is called from Build, and Build runs each time the
// statement does. A plan is shared between executions and a compiled expression
// is not, so caching here cannot leak one execution's rows, or one
// transaction's snapshot, into the next.

// drainColumn runs a subplan and collects the first column of every row.
//
// limit caps how many rows are read, so a scalar subquery can stop as soon as
// it knows there is more than one rather than draining a whole table to report
// the error; limit <= 0 reads everything.
//
// Copying out row[0] is required, not incidental: an operator's row is only
// valid until the next Next call. types.Value is a value type, so the append
// copies it.
func drainColumn(ctx context.Context, n plan.Node, tx *storage.Tx, args []types.Value, limit int) (vals []types.Value, err error) {
	op, err := Build(ctx, n, tx, args)
	if err != nil {
		return nil, err
	}
	// Join rather than replace: a close failure matters, but not enough to
	// hide whatever went wrong first.
	defer func() { err = errors.Join(err, op.Close()) }()

	for limit <= 0 || len(vals) < limit {
		row, ok, err := op.Next(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		vals = append(vals, row[0])
	}
	return vals, nil
}

func compileScalarSubquery(e *plan.ScalarSubquery, tx *storage.Tx) (Eval, error) {
	sub := e.Input
	var (
		cached types.Value
		done   bool
	)
	return func(ctx context.Context, args []types.Value, _ Row) (types.Value, error) {
		if done {
			return cached, nil
		}
		// Two rows is all it takes to know there are too many.
		vals, err := drainColumn(ctx, sub, tx, args, 2)
		if err != nil {
			return types.Value{}, err
		}
		switch len(vals) {
		case 0:
			// No rows is NULL rather than an error: asking for a value that
			// turns out not to exist is a well-formed question.
			cached = types.Null()
		case 1:
			cached = vals[0]
		default:
			return types.Value{}, pgerr.New(pgerr.CardinalityViolation,
				"more than one row returned by a subquery used as an expression")
		}
		done = true
		return cached, nil
	}, nil
}

func compileExists(e *plan.ExistsSubquery, tx *storage.Tx) (Eval, error) {
	sub, negate := e.Input, e.Negate
	var (
		found bool
		done  bool
	)
	return func(ctx context.Context, args []types.Value, _ Row) (types.Value, error) {
		if !done {
			// Only the first row is asked for. EXISTS has its answer as soon as
			// one arrives, and the operator tree is torn down without producing
			// the rest.
			ok, err := anyRow(ctx, sub, tx, args)
			if err != nil {
				return types.Value{}, err
			}
			found, done = ok, true
		}
		// EXISTS is never unknown: a row is either there or it is not, whatever
		// that row contains.
		return types.Bool(found != negate), nil
	}, nil
}

// anyRow reports whether a subplan produces at least one row. It is
// separate from drainColumn because EXISTS does not care about the select list
// at all, so a subquery of any width is legal here.
func anyRow(ctx context.Context, n plan.Node, tx *storage.Tx, args []types.Value) (found bool, err error) {
	op, err := Build(ctx, n, tx, args)
	if err != nil {
		return false, err
	}
	// Join rather than replace: a close failure matters, but not enough to
	// hide whatever went wrong first.
	defer func() { err = errors.Join(err, op.Close()) }()

	_, found, err = op.Next(ctx)
	if err != nil {
		return false, err
	}
	return found, nil
}

func compileInSubquery(e *plan.InSubquery, tx *storage.Tx) (Eval, error) {
	x, err := Compile(e.X, tx)
	if err != nil {
		return nil, err
	}
	sub, negate := e.Input, e.Negate
	var (
		vals []types.Value
		done bool
	)
	return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
		if !done {
			if vals, err = drainColumn(ctx, sub, tx, args, 0); err != nil {
				return types.Value{}, err
			}
			done = true
		}
		xv, err := x(ctx, args, row)
		if err != nil {
			return types.Value{}, err
		}
		return inResult(xv, vals, negate), nil
	}, nil
}

func compileInList(e *plan.InList, tx *storage.Tx) (Eval, error) {
	x, err := Compile(e.X, tx)
	if err != nil {
		return nil, err
	}
	list := make([]Eval, len(e.List))
	for i, item := range e.List {
		if list[i], err = Compile(item, tx); err != nil {
			return nil, err
		}
	}
	negate := e.Negate
	vals := make([]types.Value, len(list))
	return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
		xv, err := x(ctx, args, row)
		if err != nil {
			return types.Value{}, err
		}
		// The list is evaluated per row, unlike a subquery's: an element may
		// reference a column, so `a IN (b, c)` is a legal and row-dependent
		// question.
		for i, ev := range list {
			if vals[i], err = ev(ctx, args, row); err != nil {
				return types.Value{}, err
			}
		}
		return inResult(xv, vals, negate), nil
	}, nil
}

// inResult applies SQL's three-valued IN.
//
// A match is true. Without one, a NULL anywhere -- on the left or among the
// candidates -- makes the answer unknown rather than false, because the value
// that is missing might have been the matching one. That is what makes
// `x NOT IN (SELECT ...)` return nothing when the subquery yields a NULL, which
// looks like a bug to almost everyone who meets it and is the rule.
func inResult(x types.Value, vals []types.Value, negate bool) types.Value {
	if x.IsNull() {
		return types.Null()
	}
	sawNull := false
	for _, v := range vals {
		switch types.Eq(x, v) {
		case types.True:
			return types.Bool(!negate)
		case types.Unknown:
			sawNull = true
		}
	}
	if sawNull {
		return types.Null()
	}
	return types.Bool(negate)
}

func compileUnary(e *plan.Unary, tx *storage.Tx) (Eval, error) {
	x, err := Compile(e.X, tx)
	if err != nil {
		return nil, err
	}

	switch e.Op {
	case ast.OpPlus:
		return x, nil

	case ast.OpNot:
		return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
			v, err := x(ctx, args, row)
			if err != nil {
				return types.Value{}, err
			}
			// NOT UNKNOWN is UNKNOWN, which Bool3 handles and a Go bool
			// would silently turn into true.
			return v.Truth().Not().Value(), nil
		}, nil

	default: // OpNeg
		return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
			v, err := x(ctx, args, row)
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

func compileBinary(e *plan.Binary, tx *storage.Tx) (Eval, error) {
	l, err := Compile(e.L, tx)
	if err != nil {
		return nil, err
	}
	r, err := Compile(e.R, tx)
	if err != nil {
		return nil, err
	}

	// AND and OR short-circuit, so they cannot use the generic path that
	// evaluates both operands first.
	switch e.Op {
	case ast.OpAnd:
		return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
			lv, err := l(ctx, args, row)
			if err != nil {
				return types.Value{}, err
			}
			// FALSE dominates AND, so the right operand need not be evaluated.
			if lv.Truth() == types.False {
				return types.Bool(false), nil
			}
			rv, err := r(ctx, args, row)
			if err != nil {
				return types.Value{}, err
			}
			return lv.Truth().And(rv.Truth()).Value(), nil
		}, nil

	case ast.OpOr:
		return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
			lv, err := l(ctx, args, row)
			if err != nil {
				return types.Value{}, err
			}
			if lv.Truth() == types.True {
				return types.Bool(true), nil
			}
			rv, err := r(ctx, args, row)
			if err != nil {
				return types.Value{}, err
			}
			return lv.Truth().Or(rv.Truth()).Value(), nil
		}, nil
	}

	if cmp, ok := comparisons[e.Op]; ok {
		return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
			lv, rv, err := evalPair(ctx, l, r, args, row)
			if err != nil {
				return types.Value{}, err
			}
			return cmp(lv, rv).Value(), nil
		}, nil
	}

	switch e.Op {
	case ast.OpJSONField, ast.OpJSONText, ast.OpJSONContains:
		op := e.Op
		return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
			lv, rv, err := evalPair(ctx, l, r, args, row)
			if err != nil {
				return types.Value{}, err
			}
			// A NULL operand gives NULL, as every JSON operator does in
			// PostgreSQL: there is no document to look in.
			if lv.IsNull() || rv.IsNull() {
				return types.Null(), nil
			}
			switch op {
			case ast.OpJSONField:
				return types.JSONField(lv, rv), nil
			case ast.OpJSONText:
				return types.JSONText(lv, rv), nil
			default:
				return types.JSONContains(lv, rv).Value(), nil
			}
		}, nil
	}

	if e.Op == ast.OpConcat {
		return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
			lv, rv, err := evalPair(ctx, l, r, args, row)
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

	return func(ctx context.Context, args []types.Value, row Row) (types.Value, error) {
		lv, rv, err := evalPair(ctx, l, r, args, row)
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
func evalPair(ctx context.Context, l, r Eval, args []types.Value, row Row) (left, right types.Value, err error) {
	lv, err := l(ctx, args, row)
	if err != nil {
		return types.Value{}, types.Value{}, err
	}
	rv, err := r(ctx, args, row)
	if err != nil {
		return types.Value{}, types.Value{}, err
	}
	return lv, rv, nil
}
