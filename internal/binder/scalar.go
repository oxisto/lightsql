package binder

import (
	"strings"

	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/builtin"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/types"
)

// bindScalarCall resolves a call to a scalar function.
//
// The ok result distinguishes "this is not a scalar function" from "it is, and
// it is wrong". Returning an error for the first would stop the caller from
// going on to try the aggregates.
func bindScalarCall(fc *ast.FuncCall, sc *scope) (plan.Expr, bool, error) {
	name := fc.Name.Name

	// DISTINCT and * belong to aggregates. Rejecting them here rather than
	// ignoring them stops lower(DISTINCT s) from being quietly accepted as
	// lower(s).
	if fc.Distinct || fc.Star {
		if builtin.IsScalar(name) || isSpecialForm(name) {
			return nil, true, pgerr.Newf(pgerr.SyntaxError,
				"function %s does not accept DISTINCT or *", name).At(fc.Pos())
		}
		return nil, false, nil
	}

	switch lower(name) {
	case "now":
		if len(fc.Args) != 0 {
			return nil, true, pgerr.New(pgerr.UndefinedFunction,
				"function now takes no arguments").At(fc.Pos())
		}
		return &plan.Now{Kind: types.KindTimestamptz}, true, nil

	case "coalesce":
		e, err := bindCoalesce(fc, sc)
		return e, true, err
	}

	fn, ok := builtin.LookupScalar(name)
	if !ok {
		return nil, false, nil
	}

	args := make([]plan.Expr, len(fc.Args))
	kinds := make([]types.Kind, len(fc.Args))
	for i, a := range fc.Args {
		bound, err := bindExpr(a, sc)
		if err != nil {
			return nil, true, err
		}
		args[i] = bound
		kinds[i] = bound.Type()
	}

	if err := checkArity(fn.Name, len(args), fn.Min, fn.Max, fc); err != nil {
		return nil, true, err
	}
	if err := checkArgTypes(fn, kinds, fc); err != nil {
		return nil, true, err
	}
	return &plan.FuncCall{Func: fn.Name, Args: args, Kind: fn.Result(kinds)}, true, nil
}

// checkArgTypes refuses a call whose arguments the function cannot read.
//
// This is not politeness about types. A Value keeps its payload in the same
// field whatever the kind, so lower(1) does not fail: it reads the empty string
// payload of an integer and returns "", and length(1) reports 0. Refusing here
// is what keeps a wrong answer from looking like a right one, exactly as the
// same check does for sum and avg.
func checkArgTypes(fn *builtin.Scalar, kinds []types.Kind, fc *ast.FuncCall) error {
	for i, k := range kinds {
		// A NULL argument carries no type and is compatible with anything; a
		// strict function will short-circuit on it anyway.
		if k == types.KindNull {
			continue
		}
		if fn.Numeric && !k.IsNumeric() {
			return argTypeError(fn.Name, kinds, fc)
		}
		if i < len(fn.Args) && fn.Args[i] != types.KindNull && fn.Args[i] != k {
			return argTypeError(fn.Name, kinds, fc)
		}
	}
	return nil
}

// argTypeError reports the call the way PostgreSQL does, naming the argument
// types so the reader can see which overload they asked for.
func argTypeError(name string, kinds []types.Kind, fc *ast.FuncCall) error {
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = k.String()
	}
	return pgerr.Newf(pgerr.UndefinedFunction,
		"function %s(%s) does not exist", name, strings.Join(parts, ", ")).At(fc.Pos())
}

// bindCoalesce binds COALESCE, whose arguments must share a type because any
// one of them may be the answer.
func bindCoalesce(fc *ast.FuncCall, sc *scope) (plan.Expr, error) {
	if len(fc.Args) == 0 {
		return nil, pgerr.New(pgerr.UndefinedFunction,
			"function coalesce requires at least one argument").At(fc.Pos())
	}

	args := make([]plan.Expr, len(fc.Args))
	for i, a := range fc.Args {
		bound, err := bindExpr(a, sc)
		if err != nil {
			return nil, err
		}
		args[i] = bound
	}

	// The same rule a CASE uses, and for the same reason: whichever argument
	// answers, the result column has one declared type, so they have to agree
	// before the query runs rather than per row.
	kind, err := commonKind(args, fc.Pos())
	if err != nil {
		return nil, err
	}
	for i := range args {
		if args[i], err = coerce(args[i], kind, fc.Args[i].Pos()); err != nil {
			return nil, err
		}
	}
	return &plan.Coalesce{Args: args, Kind: kind}, nil
}

// isSpecialForm reports whether a name is handled outside the registry, so that
// argument-shape errors are reported for those too.
func isSpecialForm(name string) bool {
	switch lower(name) {
	case "now", "coalesce":
		return true
	}
	return false
}

func checkArity(name string, got, lo, hi int, fc *ast.FuncCall) error {
	if got < lo || (hi >= 0 && got > hi) {
		return pgerr.Newf(pgerr.UndefinedFunction,
			"function %s does not take %d arguments", name, got).At(fc.Pos())
	}
	return nil
}

// lower is strings.ToLower under a shorter name, since every function lookup
// here is case-insensitive.
func lower(s string) string { return strings.ToLower(s) }
