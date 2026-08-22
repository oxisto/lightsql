package builtin

import (
	"math"
	"strings"

	"github.com/oxisto/lightsql/internal/types"
)

// Scalar describes one scalar function: something that takes values and returns
// a value, as opposed to an Aggregate, which folds a stream of them.
type Scalar struct {
	Name string
	// Min and Max bound the argument count. Max is -1 for variadic.
	Min, Max int
	// Args is the kind each parameter requires, or KindNull for one that takes
	// anything. It is checked at bind time rather than trusted, because a Value
	// keeps its payload in the same field whatever the kind: lower(1) does not
	// fail on its own, it reads the empty string payload of an integer and
	// returns "" -- a confident wrong answer of exactly the sort sum(text) gave
	// before it was rejected.
	Args []types.Kind
	// AltArgs is a second kind each parameter will also accept, or KindNull
	// where there is none.
	//
	// PostgreSQL solves this with overloading: length(text) and length(bytea)
	// are two functions that happen to share a name. This registry is keyed by
	// name alone and cannot express that, and one alternative covers every
	// function here that needs one -- both of them count something in a string
	// that may be either characters or bytes.
	AltArgs []types.Kind
	// Numeric marks a function whose arguments must be numbers, where any of
	// the numeric kinds will do.
	Numeric bool
	// Strict marks a function that returns NULL whenever any argument is NULL,
	// which is true of almost all of them. Declaring it here rather than
	// repeating the check in every implementation is what keeps one function
	// from quietly forgetting SQL's rule -- and coalesce, which exists
	// precisely to look at a NULL, has to say so.
	Strict bool
	// Result reports the type produced for the given argument types. It is
	// consulted at bind time, so a query is typed without being run.
	Result func(args []types.Kind) types.Kind
	// Fn computes the result. Strict functions never see a NULL argument.
	Fn func(args []types.Value) (types.Value, error)
}

// scalars is the registry, keyed by lower-cased name.
var scalars = map[string]*Scalar{
	"lower": {
		Name: "lower", Min: 1, Max: 1, Strict: true, Args: []types.Kind{types.KindText},
		Result: constKind(types.KindText),
		Fn: func(a []types.Value) (types.Value, error) {
			return types.Text(strings.ToLower(a[0].AsString())), nil
		},
	},
	"upper": {
		Name: "upper", Min: 1, Max: 1, Strict: true, Args: []types.Kind{types.KindText},
		Result: constKind(types.KindText),
		Fn: func(a []types.Value) (types.Value, error) {
			return types.Text(strings.ToUpper(a[0].AsString())), nil
		},
	},
	"length": {
		Name: "length", Min: 1, Max: 1, Strict: true,
		Args:    []types.Kind{types.KindText},
		AltArgs: []types.Kind{types.KindBytea},
		Result:  constKind(types.KindInt),
		Fn: func(a []types.Value) (types.Value, error) {
			// Characters for text, bytes for bytea, as PostgreSQL does for
			// each. Counting bytes for text would make length('é') report 2;
			// counting characters for bytea would decode arbitrary bytes as
			// UTF-8 and report whatever that happened to produce.
			if a[0].Kind() == types.KindBytea {
				return types.Int(int64(len(a[0].AsString()))), nil
			}
			return types.Int(int64(len([]rune(a[0].AsString())))), nil
		},
	},
	"octet_length": {
		Name: "octet_length", Min: 1, Max: 1, Strict: true,
		Args:    []types.Kind{types.KindText},
		AltArgs: []types.Kind{types.KindBytea},
		Result:  constKind(types.KindInt),
		Fn: func(a []types.Value) (types.Value, error) {
			// Always bytes, which for text is the length of its UTF-8. This is
			// the function to reach for when the answer has to be a size rather
			// than a count of characters.
			return types.Int(int64(len(a[0].AsString()))), nil
		},
	},
	"trim": {
		Name: "trim", Min: 1, Max: 1, Strict: true, Args: []types.Kind{types.KindText},
		Result: constKind(types.KindText),
		Fn: func(a []types.Value) (types.Value, error) {
			return types.Text(strings.TrimSpace(a[0].AsString())), nil
		},
	},
	"abs": {
		Name: "abs", Min: 1, Max: 1, Strict: true, Numeric: true,
		Result: firstKind,
		Fn: func(a []types.Value) (types.Value, error) {
			if a[0].Kind() == types.KindInt {
				if n := a[0].AsInt(); n < 0 {
					return types.Int(-n), nil
				}
				return a[0], nil
			}
			return types.Float(math.Abs(a[0].AsFloat())), nil
		},
	},
	"round": {
		Name: "round", Min: 1, Max: 1, Strict: true, Numeric: true,
		Result: firstKind,
		Fn: func(a []types.Value) (types.Value, error) {
			if a[0].Kind() == types.KindInt {
				return a[0], nil
			}
			return types.Float(math.Round(a[0].AsFloat())), nil
		},
	},
	"nullif": {
		Name: "nullif", Min: 2, Max: 2,
		// Not strict: NULLIF(NULL, 1) is NULL because the first argument is,
		// not because the function refuses to look.
		Result: firstKind,
		Fn: func(a []types.Value) (types.Value, error) {
			// SQL equality, so a NULL operand makes the comparison unknown and
			// the value is returned unchanged rather than nulled.
			if types.Eq(a[0], a[1]) == types.True {
				return types.Null(), nil
			}
			return a[0], nil
		},
	},
}

func constKind(k types.Kind) func([]types.Kind) types.Kind {
	return func([]types.Kind) types.Kind { return k }
}

func firstKind(args []types.Kind) types.Kind {
	if len(args) == 0 {
		return types.KindNull
	}
	return args[0]
}

// LookupScalar returns the scalar function with the given name. The comparison
// is case-insensitive, since LOWER and lower are the same function.
func LookupScalar(name string) (*Scalar, bool) {
	s, ok := scalars[strings.ToLower(name)]
	return s, ok
}

// IsScalar reports whether name is a scalar function.
func IsScalar(name string) bool {
	_, ok := LookupScalar(name)
	return ok
}
