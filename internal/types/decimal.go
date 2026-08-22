package types

import (
	"math/big"
	"strconv"
	"strings"
)

// Decimal is an exact base-ten number: an arbitrary-precision unscaled integer
// together with the number of digits that sit after the decimal point.
//
//	value = Unscaled × 10⁻ˢᶜᵃˡᵉ
//
// It is arbitrary precision rather than a scaled int64 because the point of
// asking for DECIMAL is that the answer is right. A 64-bit payload covers money
// comfortably and then silently stops: multiply two values near the top of the
// range and the product does not fit, so the choice would be between an
// overflow error on arithmetic that is mathematically fine, and a wrong answer.
// Neither belongs in a column someone reached for because they did not trust a
// float.
//
// The cost is real and is paid on every row carrying one: a Decimal is behind a
// pointer, so it allocates, and it pushed Value from 32 bytes to 40. Both are
// deliberate; see TestValueSize.
//
// A Decimal is immutable once it is inside a Value. Values are copied
// constantly and share the pointer, so an operation returns a new Decimal
// rather than writing through this one -- the same discipline that applies to a
// row version's values in the heap, and for the same reason.
type Decimal struct {
	// Unscaled is the digits, without a decimal point. It is never nil in a
	// Decimal reachable from a Value.
	Unscaled *big.Int
	// Scale is how many of those digits fall after the point. It is not
	// negative: PostgreSQL's numeric permits a negative display scale, but
	// nothing here produces one and allowing it would double the cases every
	// operation has to think about.
	Scale int32
}

// NewDecimal returns a decimal from an unscaled integer and a scale. The
// big.Int is taken as given rather than copied, so callers must not keep a
// reference they will later write through.
func NewDecimal(unscaled *big.Int, scale int32) *Decimal {
	return &Decimal{Unscaled: unscaled, Scale: scale}
}

// DecimalFromInt returns an exact decimal for an integer.
func DecimalFromInt(i int64) *Decimal {
	return &Decimal{Unscaled: big.NewInt(i), Scale: 0}
}

// pow10 returns 10^n as a big.Int. The small exponents that dominate -- a
// column's declared scale, the difference between two scales -- come from a
// table rather than from a multiplication loop.
func pow10(n int32) *big.Int {
	if n < 0 {
		panic("types: negative power of ten")
	}
	if int(n) < len(smallPow10) {
		return smallPow10[n]
	}
	return new(big.Int).Exp(bigTen, big.NewInt(int64(n)), nil)
}

var (
	bigTen     = big.NewInt(10)
	smallPow10 = func() []*big.Int {
		out := make([]*big.Int, 40)
		for i := range out {
			out[i] = new(big.Int).Exp(bigTen, big.NewInt(int64(i)), nil)
		}
		return out
	}()
)

// rescale returns d's digits expressed at the given scale, which must be at
// least d.Scale. Widening a scale is exact; narrowing is rounding, which is
// Round's business rather than this one's.
func (d *Decimal) rescale(scale int32) *big.Int {
	if scale == d.Scale {
		return d.Unscaled
	}
	return new(big.Int).Mul(d.Unscaled, pow10(scale-d.Scale))
}

// align returns both operands' digits at a common scale, which is the wider of
// the two so that neither loses anything.
func align(a, b *Decimal) (x, y *big.Int, scale int32) {
	scale = max(a.Scale, b.Scale)
	return a.rescale(scale), b.rescale(scale), scale
}

// Add returns a + b, exactly. The result's scale is the wider of the two, which
// is what PostgreSQL does and what keeps 1.50 + 1 an amount of money rather
// than an integer.
func (d *Decimal) Add(b *Decimal) *Decimal {
	x, y, scale := align(d, b)
	return &Decimal{Unscaled: new(big.Int).Add(x, y), Scale: scale}
}

// Sub returns a - b, exactly.
func (d *Decimal) Sub(b *Decimal) *Decimal {
	x, y, scale := align(d, b)
	return &Decimal{Unscaled: new(big.Int).Sub(x, y), Scale: scale}
}

// Mul returns a × b, exactly. The scales add, as they do when multiplying by
// hand, so 1.50 × 1.50 is 2.2500 rather than 2.25.
func (d *Decimal) Mul(b *Decimal) *Decimal {
	return &Decimal{
		Unscaled: new(big.Int).Mul(d.Unscaled, b.Unscaled),
		Scale:    d.Scale + b.Scale,
	}
}

// Mod returns the remainder of d ÷ b with the quotient truncated towards zero,
// so the sign follows the dividend as it does for integers. b must not be zero.
func (d *Decimal) Mod(b *Decimal) *Decimal {
	x, y, scale := align(d, b)
	return &Decimal{Unscaled: new(big.Int).Rem(x, y), Scale: scale}
}

// Neg returns -d.
func (d *Decimal) Neg() *Decimal {
	return &Decimal{Unscaled: new(big.Int).Neg(d.Unscaled), Scale: d.Scale}
}

// IsZero reports whether d is zero at any scale.
func (d *Decimal) IsZero() bool { return d.Unscaled.Sign() == 0 }

// Sign returns -1, 0 or +1.
func (d *Decimal) Sign() int { return d.Unscaled.Sign() }

// Cmp orders two decimals, comparing their values rather than their digits, so
// that 1.5 and 1.50 are equal.
func (d *Decimal) Cmp(b *Decimal) int {
	x, y, _ := align(d, b)
	return x.Cmp(y)
}

// Round returns d at the given scale, rounding half away from zero -- the rule
// PostgreSQL uses for numeric, and the one people expect from money. Half to
// even is better for statistics and wrong for invoices.
func (d *Decimal) Round(scale int32) *Decimal {
	if scale >= d.Scale {
		return &Decimal{Unscaled: d.rescale(scale), Scale: scale}
	}

	drop := pow10(d.Scale - scale)
	q, r := new(big.Int).QuoRem(d.Unscaled, drop, new(big.Int))
	if r.Sign() != 0 {
		// QuoRem truncates towards zero and leaves the remainder with the
		// dividend's sign, so the same comparison serves both directions.
		twice := new(big.Int).Abs(r)
		twice.Lsh(twice, 1)
		if twice.Cmp(drop) >= 0 {
			if d.Unscaled.Sign() < 0 {
				q.Sub(q, bigOne)
			} else {
				q.Add(q, bigOne)
			}
		}
	}
	return &Decimal{Unscaled: q, Scale: scale}
}

var bigOne = big.NewInt(1)

// formatScaled renders an unscaled int64 at a scale, which is what rendering a
// result set does to every numeric column. It avoids building a big.Int for a
// value that never needed one.
func formatScaled(unscaled int64, scale int32) string {
	return placePoint(strconv.FormatInt(unscaled, 10), scale)
}

// String renders the decimal with exactly Scale digits after the point, so a
// value declared to two places prints as 1.50 rather than 1.5.
func (d *Decimal) String() string { return placePoint(d.Unscaled.String(), d.Scale) }

// placePoint inserts the decimal point into a run of digits.
func placePoint(digits string, scale int32) string {
	neg := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")

	var sb strings.Builder
	if neg {
		sb.WriteByte('-')
	}
	if scale <= 0 {
		sb.WriteString(digits)
		return sb.String()
	}
	// Left-pad so that a value smaller than one still has a digit before the
	// point: 5 at scale 2 is 0.05, not .05.
	if int32(len(digits)) <= scale {
		sb.WriteString("0.")
		sb.WriteString(strings.Repeat("0", int(scale)-len(digits)))
		sb.WriteString(digits)
		return sb.String()
	}
	cut := int32(len(digits)) - scale
	sb.WriteString(digits[:cut])
	sb.WriteByte('.')
	sb.WriteString(digits[cut:])
	return sb.String()
}

// Float64 returns the decimal as a float, for the paths that mix it with an
// inexact type. It is lossy by construction, which is why nothing exact goes
// through here.
func (d *Decimal) Float64() float64 {
	f, _ := new(big.Float).Quo(
		new(big.Float).SetInt(d.Unscaled),
		new(big.Float).SetInt(pow10(d.Scale)),
	).Float64()
	return f
}

// Int64 returns the decimal truncated towards zero, and whether it fitted.
func (d *Decimal) Int64() (int64, bool) {
	whole := d.Round(0)
	if !whole.Unscaled.IsInt64() {
		return 0, false
	}
	return whole.Unscaled.Int64(), true
}
