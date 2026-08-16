// Package builtin holds lightsql's function library.
//
// An aggregate is expressed as an accumulator rather than as a function over a
// slice of rows, so a group never has to be materialised: the executor feeds
// values in as it sees them and asks for the answer once. That is what keeps a
// GROUP BY over a large table to one accumulator per group rather than one
// slice per group.
package builtin

import (
	"hash/maphash"
	"strings"

	"github.com/oxisto/lightsql/internal/types"
)

// Accumulator folds a stream of values into one result.
type Accumulator interface {
	// Add takes one value. NULL handling belongs to the implementation: SQL
	// aggregates other than count(*) skip NULLs rather than propagating them,
	// which is the opposite of how NULL behaves everywhere else.
	Add(v types.Value)
	// Result reports the aggregate over everything added so far.
	Result() types.Value
}

// Aggregate describes one aggregate function.
type Aggregate struct {
	Name string
	// Numeric marks an aggregate that arithmetic only makes sense for. sum and
	// avg read the value's numeric payload, so handing one a text or boolean
	// value would produce a confident wrong number rather than an error; the
	// binder refuses those instead. count works on anything, and min and max
	// need only the total order, which is defined for every kind.
	Numeric bool
	// Result reports the type the aggregate produces for a given argument
	// type, so the binder can type the query without running it.
	Result func(arg types.Kind) types.Kind
	// New returns a fresh accumulator, one per group.
	New func() Accumulator
}

// aggregates is the registry, keyed by lower-cased name.
var aggregates = map[string]*Aggregate{
	"count": {
		Name: "count",
		// count always answers with an integer, and it is the one aggregate
		// that answers 0 rather than NULL for an empty input.
		Result: func(types.Kind) types.Kind { return types.KindInt },
		New:    func() Accumulator { return &countAgg{} },
	},
	"sum": {
		Name:    "sum",
		Numeric: true,
		Result:  numericResult,
		New:     func() Accumulator { return &sumAgg{} },
	},
	"avg": {
		Name:    "avg",
		Numeric: true,
		// avg is float even over integers: the average of 1 and 2 is 1.5, and
		// truncating it to 1 would be a silent wrong answer. PostgreSQL returns
		// numeric here, which lightsql does not have.
		Result: func(types.Kind) types.Kind { return types.KindFloat },
		New:    func() Accumulator { return &avgAgg{} },
	},
	"min": {
		Name:   "min",
		Result: sameResult,
		New:    func() Accumulator { return &extremeAgg{min: true} },
	},
	"max": {
		Name:   "max",
		Result: sameResult,
		New:    func() Accumulator { return &extremeAgg{} },
	},
}

// numericResult keeps integer sums exact and promotes a float input to float.
// PostgreSQL widens sum(int) to bigint; Value's integer is already 64-bit, so
// there is nothing to widen to.
func numericResult(arg types.Kind) types.Kind {
	if arg == types.KindFloat {
		return types.KindFloat
	}
	return types.KindInt
}

func sameResult(arg types.Kind) types.Kind { return arg }

// LookupAggregate returns the aggregate with the given name, if it is one.
// The name must be compared case-insensitively, since COUNT and count are the
// same function.
func LookupAggregate(name string) (*Aggregate, bool) {
	a, ok := aggregates[strings.ToLower(name)]
	return a, ok
}

// IsAggregate reports whether name is an aggregate function.
func IsAggregate(name string) bool {
	_, ok := LookupAggregate(name)
	return ok
}

// CountStar returns the accumulator for count(*), which counts rows and so
// never skips anything.
func CountStar() Accumulator { return &countAgg{star: true} }

type countAgg struct {
	n    int64
	star bool
}

func (a *countAgg) Add(v types.Value) {
	// count(x) counts values that are not NULL; count(*) counts rows, so the
	// value it is handed is irrelevant.
	if a.star || !v.IsNull() {
		a.n++
	}
}

func (a *countAgg) Result() types.Value { return types.Int(a.n) }

type sumAgg struct {
	i     int64
	f     float64
	float bool
	any   bool
}

func (a *sumAgg) Add(v types.Value) {
	if v.IsNull() {
		return
	}
	a.any = true
	if v.Kind() == types.KindFloat {
		// The first float makes the whole sum float, and the integer part
		// accumulated so far comes along.
		if !a.float {
			a.float = true
			a.f = float64(a.i)
		}
		a.f += v.AsFloat()
		return
	}
	if a.float {
		a.f += v.AsFloat()
		return
	}
	a.i += v.AsInt()
}

func (a *sumAgg) Result() types.Value {
	// sum over no rows is NULL, not zero. That is SQL's rule, and it is the
	// difference between "nothing was added up" and "the total is zero".
	if !a.any {
		return types.Null()
	}
	if a.float {
		return types.Float(a.f)
	}
	return types.Int(a.i)
}

type avgAgg struct {
	sum sumAgg
	n   int64
}

func (a *avgAgg) Add(v types.Value) {
	if v.IsNull() {
		return
	}
	a.sum.Add(v)
	a.n++
}

func (a *avgAgg) Result() types.Value {
	if a.n == 0 {
		return types.Null()
	}
	return types.Float(a.sum.Result().AsFloat() / float64(a.n))
}

type extremeAgg struct {
	cur types.Value
	min bool
	any bool
}

func (a *extremeAgg) Add(v types.Value) {
	if v.IsNull() {
		return
	}
	if !a.any {
		a.cur, a.any = v, true
		return
	}
	// Compare is the total order, which is what min and max want: it is
	// defined across the numeric kinds, so min(1, 1.5) does not depend on
	// which of the two arrived first.
	c := types.Compare(v, a.cur)
	if (a.min && c < 0) || (!a.min && c > 0) {
		a.cur = v
	}
}

func (a *extremeAgg) Result() types.Value {
	if !a.any {
		return types.Null()
	}
	return a.cur
}

// Distinct wraps an accumulator so that repeated values are folded in once,
// implementing count(DISTINCT x) and friends.
func Distinct(inner Accumulator) Accumulator {
	return &distinctAgg{inner: inner, seen: newValueSet()}
}

type distinctAgg struct {
	inner Accumulator
	seen  *valueSet
}

func (a *distinctAgg) Add(v types.Value) {
	// A NULL is not a value to be distinct about; every aggregate that reaches
	// here skips NULLs anyway, so passing it through would only risk a count.
	if v.IsNull() {
		return
	}
	if a.seen.add(v) {
		a.inner.Add(v)
	}
}

func (a *distinctAgg) Result() types.Value { return a.inner.Result() }

// valueSet is a set of values under SQL equality.
//
// It hashes with Value.Hash and settles ties with Compare, rather than using a
// Go map keyed on Value directly. Value is a comparable struct, so a plain map
// would compile — and would then treat the integer 1 and the float 1 as
// different, contradicting the comparison the rest of the engine uses.
type valueSet struct {
	seed    maphash.Seed
	buckets map[uint64][]types.Value
}

func newValueSet() *valueSet {
	return &valueSet{seed: maphash.MakeSeed(), buckets: make(map[uint64][]types.Value)}
}

// add records v and reports whether it was new.
func (s *valueSet) add(v types.Value) bool {
	var h maphash.Hash
	h.SetSeed(s.seed)
	v.Hash(&h)
	sum := h.Sum64()

	for _, got := range s.buckets[sum] {
		if types.Compare(got, v) == 0 {
			return false
		}
	}
	s.buckets[sum] = append(s.buckets[sum], v)
	return true
}
