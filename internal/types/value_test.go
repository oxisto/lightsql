package types

import (
	"cmp"
	"hash/maphash"
	"math"
	"testing"
	"time"
	"unsafe"
)

// TestValueSize guards the layout. Value is copied for every column of every row
// that flows through the executor, so growing it is a real cost and should be a
// deliberate decision rather than a side effect of adding a field.
//
// Exact decimals nearly cost it eight bytes. They do not, because a decimal
// that fits an int64 -- a price, a quantity, a rate -- lives in the payload
// word with its scale in the padding beside the kind, and only one too large
// for that keeps its digits in the string field. Arbitrary precision without a
// pointer, and without every other kind paying for it.
func TestValueSize(t *testing.T) {
	const want = 32
	if got := unsafe.Sizeof(Value{}); got != want {
		t.Errorf("unsafe.Sizeof(Value{}) = %d, want %d", got, want)
	}
}

// TestNullIsNotAZeroValue pins the property that makes NULL unambiguous: an
// empty string, a zero int and a false bool are all distinct from NULL.
func TestNullIsNotAZeroValue(t *testing.T) {
	for _, v := range []Value{Text(""), Int(0), Float(0), Bool(false), Bytea(nil)} {
		if v.IsNull() {
			t.Errorf("%s value %v reports IsNull", v.Kind(), v)
		}
	}
	if !Null().IsNull() {
		t.Error("Null() does not report IsNull")
	}
}

// TestComparisonIsNullAware is the direct regression test for the semantics
// ramsql gets wrong: NULL = NULL is UNKNOWN, and an ordering comparison against
// NULL is UNKNOWN rather than defaulting to true or false.
func TestComparisonIsNullAware(t *testing.T) {
	null, one := Null(), Int(1)

	tests := []struct {
		name string
		got  Bool3
	}{
		{"NULL = NULL", Eq(null, null)},
		{"NULL <> NULL", Ne(null, null)},
		{"1 = NULL", Eq(one, null)},
		{"NULL = 1", Eq(null, one)},
		{"1 > NULL", Gt(one, null)},
		{"NULL > 1", Gt(null, one)},
		{"1 < NULL", Lt(one, null)},
		{"NULL <= 1", Le(null, one)},
	}
	for _, tt := range tests {
		if tt.got != Unknown {
			t.Errorf("%s = %s, want unknown", tt.name, tt.got)
		}
	}

	// IS DISTINCT FROM is the NULL-aware form and is never unknown.
	if got := IsDistinctFrom(null, null); got != False {
		t.Errorf("NULL IS DISTINCT FROM NULL = %s, want false", got)
	}
	if got := IsDistinctFrom(null, one); got != True {
		t.Errorf("NULL IS DISTINCT FROM 1 = %s, want true", got)
	}
}

func TestComparisonOperators(t *testing.T) {
	tests := []struct {
		name string
		got  Bool3
		want Bool3
	}{
		{"1 = 1", Eq(Int(1), Int(1)), True},
		{"1 = 2", Eq(Int(1), Int(2)), False},
		{"1 <> 2", Ne(Int(1), Int(2)), True},
		{"1 < 2", Lt(Int(1), Int(2)), True},
		{"2 < 1", Lt(Int(2), Int(1)), False},
		{"1 <= 1", Le(Int(1), Int(1)), True},
		{"2 > 1", Gt(Int(2), Int(1)), True},
		{"1 >= 1", Ge(Int(1), Int(1)), True},
		{"'a' < 'b'", Lt(Text("a"), Text("b")), True},
		{"true > false", Gt(Bool(true), Bool(false)), True},
		// Integers and floats promote rather than ordering by kind.
		{"1 = 1.0", Eq(Int(1), Float(1)), True},
		{"1 < 1.5", Lt(Int(1), Float(1.5)), True},
		{"2.0 > 1", Gt(Float(2), Int(1)), True},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %s, want %s", tt.name, tt.got, tt.want)
		}
	}
}

// TestBool3TruthTables checks AND, OR and NOT against SQL's definitions, where
// FALSE dominates AND and TRUE dominates OR even in the presence of UNKNOWN.
func TestBool3TruthTables(t *testing.T) {
	all := []Bool3{True, False, Unknown}

	wantAnd := map[[2]Bool3]Bool3{
		{True, True}: True, {True, False}: False, {True, Unknown}: Unknown,
		{False, True}: False, {False, False}: False, {False, Unknown}: False,
		{Unknown, True}: Unknown, {Unknown, False}: False, {Unknown, Unknown}: Unknown,
	}
	wantOr := map[[2]Bool3]Bool3{
		{True, True}: True, {True, False}: True, {True, Unknown}: True,
		{False, True}: True, {False, False}: False, {False, Unknown}: Unknown,
		{Unknown, True}: True, {Unknown, False}: Unknown, {Unknown, Unknown}: Unknown,
	}
	for _, a := range all {
		for _, b := range all {
			if got, want := a.And(b), wantAnd[[2]Bool3{a, b}]; got != want {
				t.Errorf("%s AND %s = %s, want %s", a, b, got, want)
			}
			if got, want := a.Or(b), wantOr[[2]Bool3{a, b}]; got != want {
				t.Errorf("%s OR %s = %s, want %s", a, b, got, want)
			}
		}
	}

	wantNot := map[Bool3]Bool3{True: False, False: True, Unknown: Unknown}
	for _, a := range all {
		if got, want := a.Not(), wantNot[a]; got != want {
			t.Errorf("NOT %s = %s, want %s", a, got, want)
		}
	}

	// Only TRUE passes a filter; this is what keeps UNKNOWN rows out of a
	// WHERE clause without a separate NULL check at every call site.
	if Unknown.IsTrue() || False.IsTrue() || !True.IsTrue() {
		t.Error("IsTrue does not select exactly True")
	}
}

// TestCompareIsATotalOrder checks the ordering used by indexes and ORDER BY,
// which must order every pair — including NULLs — and must never report
// unknown.
func TestCompareIsATotalOrder(t *testing.T) {
	// PostgreSQL's default for ASC is NULLS LAST, and NaN sorts above all
	// other floats.
	ordered := []Value{Float(math.Inf(-1)), Int(-1), Int(0), Float(0.5), Int(1),
		Float(math.Inf(1)), Float(math.NaN()), Null()}

	for i := range ordered {
		for j := range ordered {
			got := Compare(ordered[i], ordered[j])
			want := cmp.Compare(i, j)
			if got != want {
				t.Errorf("Compare(%v, %v) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

// TestEqualGroupsNulls documents the deliberate difference between grouping
// equality and SQL equality: GROUP BY and DISTINCT must put all NULLs in one
// group, even though NULL = NULL is unknown.
func TestEqualGroupsNulls(t *testing.T) {
	if !Equal(Null(), Null()) {
		t.Error("Equal(NULL, NULL) = false, want true for grouping")
	}
	if Eq(Null(), Null()) != Unknown {
		t.Error("Eq(NULL, NULL) should still be unknown")
	}
}

// TestHashDoesNotConflateTypes is the regression test for hashing a formatted
// string, which makes int64(1), the text "1" and true collide.
func TestHashDoesNotConflateTypes(t *testing.T) {
	seed := maphash.MakeSeed()
	sum := func(v Value) uint64 {
		var h maphash.Hash
		h.SetSeed(seed)
		v.Hash(&h)
		return h.Sum64()
	}

	distinct := []Value{Int(1), Text("1"), Bool(true), Bytea([]byte("1")),
		Timestamp(1), Date(1), Null()}
	seen := make(map[uint64]Value, len(distinct))
	for _, v := range distinct {
		s := sum(v)
		if prev, ok := seen[s]; ok {
			t.Errorf("%s(%v) and %s(%v) hash to the same value",
				v.Kind(), v, prev.Kind(), prev)
		}
		seen[s] = v
	}

	// Values that compare equal must hash equally, or hash joins lose rows.
	for _, pair := range [][2]Value{
		{Int(1), Float(1)},
		{Int(-7), Float(-7)},
	} {
		if sum(pair[0]) != sum(pair[1]) {
			t.Errorf("%v and %v compare equal but hash differently", pair[0], pair[1])
		}
	}
}

func TestTruth(t *testing.T) {
	tests := []struct {
		v    Value
		want Bool3
	}{
		{Bool(true), True},
		{Bool(false), False},
		{Null(), Unknown},
		// A non-boolean in a boolean context is a binder error; at runtime it
		// degrades to unknown rather than panicking mid-query.
		{Int(1), Unknown},
		{Text("t"), Unknown},
	}
	for _, tt := range tests {
		if got := tt.v.Truth(); got != tt.want {
			t.Errorf("Truth(%s %v) = %s, want %s", tt.v.Kind(), tt.v, got, tt.want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		v    Value
		want string
	}{
		{"int", Int(-42), "-42"},
		{"float", Float(1.5), "1.5"},
		{"text", Text("hi"), "hi"},
		{"bool", Bool(true), "true"},
		{"null", Null(), "NULL"},
		{"date", Date(0), "1970-01-01"},
		{"timestamp", Timestamp(0), "1970-01-01 00:00:00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}

	if got := Int(-42).AsInt(); got != -42 {
		t.Errorf("AsInt round trip = %d, want -42", got)
	}
	if got := Bytea([]byte{1, 2, 3}); string(got.AsBytes()) != "\x01\x02\x03" {
		t.Errorf("Bytea round trip = %v", got.AsBytes())
	}
}

// BenchmarkValueNoAlloc documents the point of the struct layout: putting a
// scalar into a Value must not allocate, unlike boxing into an any.
func BenchmarkValueNoAlloc(b *testing.B) {
	var sink Value
	for i := 0; b.Loop(); i++ {
		sink = Int(int64(i))
	}
	_ = sink
}

// TestTimeValueRespectsTheKind covers the trap the Value layout sets: a date
// counts days, a time counts microseconds since midnight, and a timestamp
// counts microseconds since the epoch, but all three live in the same field.
// Writing the wrong unit does not fail, it produces a confident wrong answer --
// microseconds read as days is a date some three billion years hence.
func TestTimeValueRespectsTheKind(t *testing.T) {
	// 2026-08-22T10:30:45.123456Z, and the same instant before the epoch.
	at := time.Date(2026, 8, 22, 10, 30, 45, 123456000, time.UTC)
	before := time.Date(1969, 3, 4, 5, 6, 7, 0, time.UTC)

	tests := []struct {
		name string
		in   time.Time
		kind Kind
		want time.Time
	}{
		{"timestamp keeps the instant", at, KindTimestamp, at},
		{"timestamptz keeps the instant", at, KindTimestamptz, at},
		{"date drops the time of day", at, KindDate, at.Truncate(24 * time.Hour)},
		{"time drops the date", at, KindTime,
			time.Date(1970, 1, 1, 10, 30, 45, 123456000, time.UTC)},

		// Before the epoch the counts go negative, and Go's integer division
		// truncates towards zero. A date that rounded the wrong way would land
		// on the following midnight, which no test dated after 1970 would show.
		{"a date before the epoch rounds towards the past", before, KindDate,
			time.Date(1969, 3, 4, 0, 0, 0, 0, time.UTC)},
		{"a time before the epoch is still a time of day", before, KindTime,
			time.Date(1970, 1, 1, 5, 6, 7, 0, time.UTC)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := TimeValue(tt.in, tt.kind)
			if v.Kind() != tt.kind {
				t.Fatalf("kind = %s, want %s", v.Kind(), tt.kind)
			}
			if got := v.AsTime(); !got.Equal(tt.want) {
				t.Errorf("TimeValue(%s, %s).AsTime() = %s, want %s",
					tt.in.Format(time.RFC3339Nano), tt.kind,
					got.Format(time.RFC3339Nano), tt.want.Format(time.RFC3339Nano))
			}
		})
	}
}
