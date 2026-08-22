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
	"slices"
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
		// avg is never integer: the average of 1 and 2 is 1.5, and truncating
		// it to 1 would be a silent wrong answer. Over exact inputs the answer
		// stays exact, as it does in PostgreSQL, where avg(int) is numeric.
		Result: func(arg types.Kind) types.Kind {
			if arg == types.KindFloat {
				return types.KindFloat
			}
			return types.KindNumeric
		},
		New: func() Accumulator { return &avgAgg{} },
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
	switch arg {
	case types.KindFloat:
		return types.KindFloat
	case types.KindNumeric:
		// An exact input gives an exact total. Summing a column of prices
		// through float64 is the arithmetic people reach for DECIMAL to escape,
		// and doing it inside the aggregate would put it right back.
		return types.KindNumeric
	default:
		return types.KindInt
	}
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

// sumAgg totals a column in whichever of the three numeric worlds the input
// lives in, widening as it goes: integers stay integers until a decimal or a
// float appears, and a float makes the rest of the sum inexact for good.
//
// The order is one-way. Once a total has been through a float it cannot become
// exact again, so there is no path back from float to decimal.
type sumAgg struct {
	i     int64
	dec   types.Value
	f     float64
	exact bool // the running total is in dec rather than i
	float bool
	any   bool
}

func (a *sumAgg) Add(v types.Value) {
	if v.IsNull() {
		return
	}
	a.any = true

	switch {
	case a.float:
		a.f += v.AsFloat()

	case v.Kind() == types.KindFloat:
		// The first float makes the whole sum float, and whatever was
		// accumulated so far comes along.
		a.float = true
		if a.exact {
			a.f = a.dec.AsFloat()
		} else {
			a.f = float64(a.i)
		}
		a.f += v.AsFloat()

	case a.exact:
		a.dec = types.AddNumeric(a.dec, toNumeric(v))

	case v.Kind() == types.KindNumeric:
		// The first decimal moves the running integer total onto the exact
		// side, where it is represented without loss.
		a.exact = true
		a.dec = types.AddNumeric(toNumeric(types.Int(a.i)), v)

	default:
		a.i += v.AsInt()
	}
}

// toNumeric views an integer as a decimal so the two exact kinds can be added.
func toNumeric(v types.Value) types.Value {
	if v.Kind() == types.KindNumeric {
		return v
	}
	out, err := types.Cast(v, types.KindNumeric)
	if err != nil {
		return types.Numeric(types.DecimalFromInt(0))
	}
	return out
}

func (a *sumAgg) Result() types.Value {
	// sum over no rows is NULL, not zero. That is SQL's rule, and it is the
	// difference between "nothing was added up" and "the total is zero".
	if !a.any {
		return types.Null()
	}
	switch {
	case a.float:
		return types.Float(a.f)
	case a.exact:
		return a.dec
	default:
		return types.Int(a.i)
	}
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
	total := a.sum.Result()
	if total.Kind() == types.KindFloat {
		return types.Float(total.AsFloat() / float64(a.n))
	}
	// Exact input, exact answer: the division picks its scale by PostgreSQL's
	// rule, so avg over two prices has enough places to be worth having.
	q, err := types.DivNumeric(toNumeric(total), toNumeric(types.Int(a.n)))
	if err != nil {
		return types.Float(total.AsFloat() / float64(a.n))
	}
	return q
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
	return &distinctAgg{inner: inner, seen: NewKeySet()}
}

type distinctAgg struct {
	inner Accumulator
	seen  *KeySet
	one   [1]types.Value
}

func (a *distinctAgg) Add(v types.Value) {
	// A NULL is not a value to be distinct about; every aggregate that reaches
	// here skips NULLs anyway, so passing it through would only risk a count.
	if v.IsNull() {
		return
	}
	a.one[0] = v
	if a.seen.Add(a.one[:]) {
		a.inner.Add(v)
	}
}

func (a *distinctAgg) Result() types.Value { return a.inner.Result() }

// KeySet is a set of value tuples under SQL equality.
//
// It hashes with Value.Hash and settles ties with Compare, rather than using a
// Go map keyed on Value directly. Value is a comparable struct, so a plain map
// would compile — and would then treat the integer 1 and the float 1 as
// different, contradicting the comparison the rest of the engine uses.
//
// Compare rather than Eq is also what makes a NULL key group with other NULLs
// instead of forming a group of its own, which is what both GROUP BY and
// DISTINCT require.
type KeySet struct {
	seed    maphash.Seed
	buckets map[uint64][][]types.Value
}

// NewKeySet returns an empty set.
func NewKeySet() *KeySet {
	return &KeySet{seed: maphash.MakeSeed(), buckets: make(map[uint64][][]types.Value)}
}

// KeyCounts is a KeySet that remembers how many times it saw each key.
//
// INTERSECT ALL and EXCEPT ALL are multiset operations: with three copies of a
// row on the left and one on the right, INTERSECT ALL yields one and EXCEPT ALL
// yields two. Presence alone cannot answer either, which is why this exists
// beside KeySet rather than in place of it -- the distinct forms only ever need
// presence, and counting for them would be work with no result.
type KeyCounts struct {
	seed    maphash.Seed
	buckets map[uint64][]keyCount
}

type keyCount struct {
	key []types.Value
	n   int
}

func NewKeyCounts() *KeyCounts {
	return &KeyCounts{seed: maphash.MakeSeed(), buckets: make(map[uint64][]keyCount)}
}

// Add records one occurrence of key.
func (c *KeyCounts) Add(key []types.Value) {
	sum := c.hash(key)
	for i := range c.buckets[sum] {
		if sameKey(c.buckets[sum][i].key, key) {
			c.buckets[sum][i].n++
			return
		}
	}
	c.buckets[sum] = append(c.buckets[sum], keyCount{key: slices.Clone(key), n: 1})
}

// Take removes one occurrence of key, reporting whether there was one.
//
// This is what makes the ALL forms multiset operations rather than set ones:
// each row on the left consumes at most one matching row on the right.
func (c *KeyCounts) Take(key []types.Value) bool {
	sum := c.hash(key)
	for i := range c.buckets[sum] {
		if sameKey(c.buckets[sum][i].key, key) && c.buckets[sum][i].n > 0 {
			c.buckets[sum][i].n--
			return true
		}
	}
	return false
}

// Has reports whether key was ever recorded, however many times.
func (c *KeyCounts) Has(key []types.Value) bool {
	sum := c.hash(key)
	for i := range c.buckets[sum] {
		if sameKey(c.buckets[sum][i].key, key) {
			return true
		}
	}
	return false
}

func (c *KeyCounts) hash(key []types.Value) uint64 {
	var h maphash.Hash
	h.SetSeed(c.seed)
	for _, v := range key {
		v.Hash(&h)
	}
	return h.Sum64()
}

// Add records key and reports whether it was not already present. The key is
// copied, so the caller may reuse the slice for the next row.
func (s *KeySet) Add(key []types.Value) bool {
	var h maphash.Hash
	h.SetSeed(s.seed)
	for _, v := range key {
		v.Hash(&h)
	}
	sum := h.Sum64()

	for _, got := range s.buckets[sum] {
		if sameKey(got, key) {
			return false
		}
	}
	s.buckets[sum] = append(s.buckets[sum], slices.Clone(key))
	return true
}

func sameKey(a, b []types.Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if types.Compare(a[i], b[i]) != 0 {
			return false
		}
	}
	return true
}
