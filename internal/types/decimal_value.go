package types

import (
	"math/big"
	"strconv"
)

// A decimal has two representations inside a Value, and this file is the only
// place that knows it.
//
// Most decimals are small: a price, a quantity, a rate. Those live in the
// payload word as an unscaled int64, with the scale in the byte beside the kind
// — the seven bytes there are padding either way, so it is free. Nothing
// allocates, and Value stays the 32 bytes every other kind pays for.
//
// A decimal too large for an int64 keeps its digits in the string field, which
// numeric otherwise leaves empty. Arithmetic on one goes through big.Int, and
// so does arithmetic that overflows on the way.
//
// Two representations of one kind is the thing most likely to produce a
// confident wrong answer here, so the rule is narrow and absolute: nothing
// outside this file reads n or s for a numeric. Everything goes through
// smallDecimal or AsDecimal, and TestDecimalPathsAgree runs every case through
// both.

// maxSmallScale bounds the scale a small decimal may carry, so that rescaling
// two operands to a common scale cannot itself overflow before the arithmetic
// has a chance to. Anything beyond it takes the big path, where there is no
// limit.
const maxSmallScale = 18

// Numeric returns a value holding an exact decimal, inline when it fits.
func Numeric(d *Decimal) Value {
	if d.Scale >= 0 && d.Scale <= maxSmallScale && d.Unscaled.IsInt64() {
		return Value{k: KindNumeric, scale: uint8(d.Scale), n: uint64(d.Unscaled.Int64())}
	}
	return Value{k: KindNumeric, scale: scaleTag, s: encodeDecimal(d)}
}

// scaleTag marks a numeric whose payload is in the string field. It is outside
// the range of any scale a small decimal may carry, so the two cases can never
// be confused for one another.
const scaleTag uint8 = 255

// encodeDecimal renders a large decimal as its scale, a colon, then its digits.
// Text rather than big.Int's own encoding, for the same reason the log uses
// text: that encoding is not a contract anyone owes us.
func encodeDecimal(d *Decimal) string {
	return strconv.FormatInt(int64(d.Scale), 10) + ":" + d.Unscaled.String()
}

// smallDecimal returns the inline payload and whether the value has one.
func (v Value) smallDecimal() (unscaled int64, scale int32, ok bool) {
	if v.k != KindNumeric || v.scale == scaleTag {
		return 0, 0, false
	}
	return int64(v.n), int32(v.scale), true
}

// AsDecimal returns the decimal payload, allocating for a small value only when
// something actually needs the general form.
func (v Value) AsDecimal() *Decimal {
	if v.k != KindNumeric {
		return nil
	}
	if unscaled, scale, ok := v.smallDecimal(); ok {
		return &Decimal{Unscaled: big.NewInt(unscaled), Scale: scale}
	}
	scaleText, digits, _ := cut(v.s, ':')
	scale, _ := strconv.ParseInt(scaleText, 10, 32)
	unscaled, _ := new(big.Int).SetString(digits, 10)
	if unscaled == nil {
		unscaled = new(big.Int)
	}
	return &Decimal{Unscaled: unscaled, Scale: int32(scale)}
}

func cut(s string, sep byte) (before, after string, found bool) {
	for i := range len(s) {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// numericString renders a numeric without building a Decimal for the common
// case, since rendering is what a scan of a result set does to every row.
func (v Value) numericString() string {
	unscaled, scale, ok := v.smallDecimal()
	if !ok {
		return v.AsDecimal().String()
	}
	return formatScaled(unscaled, scale)
}

// AddNumeric returns a + b, both known to be exact numeric values.
//
// The fast path is plain integer arithmetic once the scales agree, which is
// what nearly every row does. It falls through to the general form on overflow
// rather than wrapping, so the answer is the same either way and only the cost
// differs.
func AddNumeric(a, b Value) Value {
	if x, y, scale, ok := alignSmall(a, b); ok {
		if sum, ok := addNoOverflow(x, y); ok {
			return Value{k: KindNumeric, scale: uint8(scale), n: uint64(sum)}
		}
	}
	return Numeric(a.AsDecimal().Add(b.AsDecimal()))
}

// SubNumeric returns a - b.
func SubNumeric(a, b Value) Value {
	if x, y, scale, ok := alignSmall(a, b); ok {
		if diff, ok := addNoOverflow(x, -y); ok && y != minInt64 {
			return Value{k: KindNumeric, scale: uint8(scale), n: uint64(diff)}
		}
	}
	return Numeric(a.AsDecimal().Sub(b.AsDecimal()))
}

// MulNumeric returns a × b. The scales add, so the result may need a wider
// scale than either operand and takes the general path when it does.
func MulNumeric(a, b Value) Value {
	x, xs, okx := a.smallDecimal()
	y, ys, oky := b.smallDecimal()
	if okx && oky && xs+ys <= maxSmallScale {
		if prod, ok := mulNoOverflow(x, y); ok {
			return Value{k: KindNumeric, scale: uint8(xs + ys), n: uint64(prod)}
		}
	}
	return Numeric(a.AsDecimal().Mul(b.AsDecimal()))
}

// DivNumeric returns a ÷ b at the scale PostgreSQL would choose.
//
// There is no fast path: the result scale depends on the magnitudes of both
// operands, and the quotient needs more digits than either of them carries. The
// general form is doing real work here rather than paying for generality.
func DivNumeric(a, b Value) (Value, error) {
	q, err := a.AsDecimal().Div(b.AsDecimal())
	if err != nil {
		return Value{}, err
	}
	return Numeric(q), nil
}

// NegNumeric returns -a.
func NegNumeric(a Value) Value {
	if x, scale, ok := a.smallDecimal(); ok && x != minInt64 {
		return Value{k: KindNumeric, scale: uint8(scale), n: uint64(-x)}
	}
	return Numeric(a.AsDecimal().Neg())
}

// cmpNumeric orders two exact numeric values.
func cmpNumeric(a, b Value) int {
	if x, y, _, ok := alignSmall(a, b); ok {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		default:
			return 0
		}
	}
	return a.AsDecimal().Cmp(b.AsDecimal())
}

// alignSmall brings two small decimals to a common scale, reporting false when
// either is not small or when widening would overflow.
func alignSmall(a, b Value) (x, y int64, scale int32, ok bool) {
	x, xs, okx := a.smallDecimal()
	y, ys, oky := b.smallDecimal()
	if !okx || !oky {
		return 0, 0, 0, false
	}
	scale = max(xs, ys)
	if x, ok = scaleNoOverflow(x, scale-xs); !ok {
		return 0, 0, 0, false
	}
	if y, ok = scaleNoOverflow(y, scale-ys); !ok {
		return 0, 0, 0, false
	}
	return x, y, scale, true
}

const (
	minInt64 = -1 << 63
	maxInt64 = 1<<63 - 1
)

// scaleNoOverflow multiplies by a power of ten, reporting false rather than
// wrapping. Wrapping here would be the worst possible failure: a large positive
// amount becoming a large negative one, silently.
func scaleNoOverflow(x int64, by int32) (int64, bool) {
	for range by {
		var ok bool
		if x, ok = mulNoOverflow(x, 10); !ok {
			return 0, false
		}
	}
	return x, true
}

func addNoOverflow(x, y int64) (int64, bool) {
	sum := x + y
	// The sum overflows exactly when both operands share a sign and the result
	// does not.
	if (x > 0 && y > 0 && sum < 0) || (x < 0 && y < 0 && sum >= 0) {
		return 0, false
	}
	return sum, true
}

func mulNoOverflow(x, y int64) (int64, bool) {
	if x == 0 || y == 0 {
		return 0, true
	}
	if x == minInt64 || y == minInt64 {
		return 0, false
	}
	prod := x * y
	// Dividing back is the check that costs nothing to get right, unlike
	// reasoning about bit widths.
	if prod/y != x {
		return 0, false
	}
	return prod, true
}
