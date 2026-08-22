package types

import (
	"hash/maphash"
	"math/big"
	"testing"
)

func mustDec(t *testing.T, s string) *Decimal {
	t.Helper()
	d, err := ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return d
}

// TestDecimalParseAndRender pins that the digits written are the digits kept.
// A column declared to two places should read back as it was entered, so the
// scale is what was written rather than a normalised one.
func TestDecimalParseAndRender(t *testing.T) {
	tests := []struct{ in, want string }{
		{"0", "0"},
		{"1", "1"},
		{"-1", "-1"},
		{"1.5", "1.5"},
		// Trailing zeros are significant: 1.50 is a price to the penny.
		{"1.50", "1.50"},
		{"0.05", "0.05"},
		{"-0.05", "-0.05"},
		{".5", "0.5"},
		{"+2.25", "2.25"},
		// An exponent shifts the point rather than being carried along.
		{"1.5e2", "150"},
		{"1.5e-2", "0.015"},
		{"15e-1", "1.5"},
		// Arbitrary precision is the whole point: this is well past float64.
		{"123456789012345678901234567890.123456789", "123456789012345678901234567890.123456789"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := mustDec(t, tt.in).String(); got != tt.want {
				t.Errorf("ParseDecimal(%q).String() = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDecimalParseErrors(t *testing.T) {
	for _, in := range []string{"", " ", ".", "-", "abc", "1.2.3", "1e", "1e+", "1e2x", "--1", "1..2"} {
		if d, err := ParseDecimal(in); err == nil {
			t.Errorf("ParseDecimal(%q) = %s, want an error", in, d)
		}
	}
}

// TestDecimalArithmeticIsExact covers the case a float cannot do, which is the
// reason this type exists: 0.1 + 0.2 is 0.3 and not 0.30000000000000004.
func TestDecimalArithmeticIsExact(t *testing.T) {
	tests := []struct {
		name    string
		a, b    string
		op      func(a, b *Decimal) *Decimal
		want    string
		wantErr bool
	}{
		{name: "the float that cannot", a: "0.1", b: "0.2",
			op: (*Decimal).Add, want: "0.3"},
		{name: "scales widen to the larger", a: "1.50", b: "1",
			op: (*Decimal).Add, want: "2.50"},
		{name: "subtract", a: "1.00", b: "1.005",
			op: (*Decimal).Sub, want: "-0.005"},
		// Scales add on multiplication, as they do on paper.
		{name: "multiply", a: "1.50", b: "1.50",
			op: (*Decimal).Mul, want: "2.2500"},
		{name: "multiply past float64", a: "9007199254740993", b: "3",
			op: (*Decimal).Mul, want: "27021597764222979"},
		{name: "negative", a: "-2.5", b: "1.25",
			op: (*Decimal).Mul, want: "-3.125"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.op(mustDec(t, tt.a), mustDec(t, tt.b))
			if got.String() != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// TestDecimalDivisionScale pins PostgreSQL's rule, which is not "some fixed
// number of places": the result carries at least sixteen significant digits, so
// how many places that is depends on how many digits land left of the point.
//
// The expectations are derived from select_div_scale rather than from
// intuition. Reasoning in significant digits gets 100/3 wrong, because
// PostgreSQL counts in base-10000 groups and therefore moves in steps of four.
func TestDecimalDivisionScale(t *testing.T) {
	tests := []struct{ a, b, want string }{
		{"1", "3", "0.33333333333333333333"},
		{"10", "3", "3.3333333333333333"},
		{"100", "3", "33.3333333333333333"},
		{"1", "7", "0.14285714285714285714"},
		{"7.5", "2.5", "3.0000000000000000"},
		{"-1", "3", "-0.33333333333333333333"},
		// The result is never less precise than either operand.
		{"1.000000000000000000000", "1", "1.000000000000000000000"},
		// Rounding of the last place is half away from zero, not truncation.
		{"2", "3", "0.66666666666666666667"},
		{"-2", "3", "-0.66666666666666666667"},
		{"0", "3", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.a+"/"+tt.b, func(t *testing.T) {
			got, err := mustDec(t, tt.a).Div(mustDec(t, tt.b))
			if err != nil {
				t.Fatalf("Div: %v", err)
			}
			if got.String() != tt.want {
				t.Errorf("%s / %s = %s, want %s", tt.a, tt.b, got, tt.want)
			}
		})
	}

	if _, err := mustDec(t, "1").Div(mustDec(t, "0")); err == nil {
		t.Error("dividing by zero succeeded")
	}
	if _, err := mustDec(t, "1").Div(mustDec(t, "0.000")); err == nil {
		t.Error("dividing by a zero with a scale succeeded")
	}
}

// TestDecimalRounding pins half away from zero, which is what money expects.
// Half to even is better for statistics and wrong for invoices.
func TestDecimalRounding(t *testing.T) {
	tests := []struct {
		in    string
		scale int32
		want  string
	}{
		{"1.5", 0, "2"},
		{"2.5", 0, "3"},
		{"-1.5", 0, "-2"},
		{"-2.5", 0, "-3"},
		{"1.4", 0, "1"},
		{"1.005", 2, "1.01"},
		{"1.004", 2, "1.00"},
		{"-1.005", 2, "-1.01"},
		// Widening is exact and pads.
		{"1.5", 3, "1.500"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := mustDec(t, tt.in).Round(tt.scale).String(); got != tt.want {
				t.Errorf("Round(%s, %d) = %s, want %s", tt.in, tt.scale, got, tt.want)
			}
		})
	}
}

// TestDecimalComparesByValue pins that a scale is a display property, not part
// of the value: 1.5 and 1.50 are the same number.
func TestDecimalComparesByValue(t *testing.T) {
	if c := mustDec(t, "1.5").Cmp(mustDec(t, "1.50")); c != 0 {
		t.Errorf("1.5 vs 1.50 = %d, want 0", c)
	}
	if c := mustDec(t, "1.5").Cmp(mustDec(t, "1.51")); c >= 0 {
		t.Errorf("1.5 vs 1.51 = %d, want negative", c)
	}
	if c := mustDec(t, "-0.0").Cmp(mustDec(t, "0")); c != 0 {
		t.Errorf("-0.0 vs 0 = %d, want 0", c)
	}
}

// TestNumericComparesExactlyAcrossKinds is the invariant a new numeric kind is
// most likely to break. Compare promotes across the numeric kinds, so a decimal
// has to compare exactly against an integer -- going through float64 would make
// two decimals seventeen digits apart compare equal, which is exactly what the
// column was chosen to prevent.
func TestNumericComparesExactlyAcrossKinds(t *testing.T) {
	big1 := Numeric(NewDecimal(mustBig(t, "9007199254740993"), 0)) // 2^53 + 1
	big2 := Numeric(NewDecimal(mustBig(t, "9007199254740992"), 0)) // 2^53

	if Compare(big1, big2) <= 0 {
		t.Error("two decimals either side of 2^53 compared equal or backwards")
	}
	if Compare(Numeric(mustDec(t, "1.00")), Int(1)) != 0 {
		t.Error("1.00 and the integer 1 are not equal")
	}
	if Compare(Numeric(mustDec(t, "1.5")), Float(1.5)) != 0 {
		t.Error("1.5 as a decimal and as a float are not equal")
	}
	if Compare(Numeric(mustDec(t, "2")), Int(1)) <= 0 {
		t.Error("2 is not greater than 1 across kinds")
	}
}

// TestNumericHashesWithItsEquals is invariant six: equal values must hash
// equally, or a GROUP BY puts the same number in two groups. Compare treats the
// numeric kinds as mutually comparable, so every pair it calls equal has to
// agree here too.
func TestNumericHashesWithItsEquals(t *testing.T) {
	seed := maphash.MakeSeed()
	hash := func(v Value) uint64 {
		var h maphash.Hash
		h.SetSeed(seed)
		v.Hash(&h)
		return h.Sum64()
	}

	groups := [][]Value{
		{Int(1), Float(1), Numeric(mustDec(t, "1")), Numeric(mustDec(t, "1.00"))},
		{Float(1.5), Numeric(mustDec(t, "1.5")), Numeric(mustDec(t, "1.500"))},
		{Int(-7), Float(-7), Numeric(mustDec(t, "-7.0"))},
		{Int(0), Float(0), Numeric(mustDec(t, "0.000"))},
	}
	for _, group := range groups {
		for _, v := range group[1:] {
			if Compare(group[0], v) != 0 {
				t.Fatalf("%v and %v are not equal, so this group is wrong", group[0], v)
			}
			if hash(group[0]) != hash(v) {
				t.Errorf("%s %v and %s %v are equal but hash differently",
					group[0].Kind(), group[0], v.Kind(), v)
			}
		}
	}
}

func mustBig(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("not a number: %s", s)
	}
	return n
}

// TestNumericCasts covers the conversions at the edges of the exact world.
func TestNumericCasts(t *testing.T) {
	// A float carries into a decimal by its shortest round-tripping text, not
	// by its exact binary value: 0.1 should become 0.1, not 0.1000000000000000055.
	got, err := Cast(Float(0.1), KindNumeric)
	if err != nil {
		t.Fatalf("Cast: %v", err)
	}
	if s := got.String(); s != "0.1" {
		t.Errorf("0.1 as a float cast to numeric is %s, want 0.1", s)
	}

	// Assigning a decimal to an integer column rounds, as PostgreSQL does.
	for _, tt := range []struct {
		in   string
		want int64
	}{{"1.4", 1}, {"1.5", 2}, {"-1.5", -2}} {
		v, err := Cast(Numeric(mustDec(t, tt.in)), KindInt)
		if err != nil {
			t.Fatalf("Cast(%s): %v", tt.in, err)
		}
		if v.AsInt() != tt.want {
			t.Errorf("%s to integer = %d, want %d", tt.in, v.AsInt(), tt.want)
		}
	}

	if v, err := Cast(Text("2.50"), KindNumeric); err != nil || v.String() != "2.50" {
		t.Errorf("text to numeric = %v, %v; want 2.50", v, err)
	}
	if _, err := Cast(Text("nope"), KindNumeric); err == nil {
		t.Error("casting nonsense to numeric succeeded")
	}
}
