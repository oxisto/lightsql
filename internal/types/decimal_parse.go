package types

import (
	"math/big"
	"strings"
)

// ParseDecimal reads a decimal from its SQL text, keeping every digit written.
//
// The scale is the number of digits after the point as written, not a
// normalised one: '1.50' is scale 2 and stays scale 2, because a column
// declared to two places should read back the way it was entered. An exponent
// shifts the point rather than being kept, since nothing downstream carries one.
func ParseDecimal(s string) (*Decimal, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return nil, &ErrCast{From: KindText, To: KindNumeric, Val: s}
	}

	mant, exp, err := splitExponent(text)
	if err != nil {
		return nil, &ErrCast{From: KindText, To: KindNumeric, Val: s}
	}

	neg := false
	switch {
	case strings.HasPrefix(mant, "-"):
		neg, mant = true, mant[1:]
	case strings.HasPrefix(mant, "+"):
		mant = mant[1:]
	}

	intPart, fracPart, hasPoint := strings.Cut(mant, ".")
	if intPart == "" && (!hasPoint || fracPart == "") {
		return nil, &ErrCast{From: KindText, To: KindNumeric, Val: s}
	}
	digits := intPart + fracPart
	if !allDigits(digits) {
		return nil, &ErrCast{From: KindText, To: KindNumeric, Val: s}
	}

	unscaled, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, &ErrCast{From: KindText, To: KindNumeric, Val: s}
	}
	if neg {
		unscaled.Neg(unscaled)
	}

	scale := int32(len(fracPart)) - exp
	d := &Decimal{Unscaled: unscaled, Scale: scale}
	if scale < 0 {
		// A negative scale means the exponent moved the point past the last
		// digit. The zeros are made explicit rather than represented by the
		// scale, so that every Decimal in circulation has a scale of at least
		// nought and no operation has to consider the other case.
		d = &Decimal{Unscaled: new(big.Int).Mul(unscaled, pow10(-scale)), Scale: 0}
	}
	return d, nil
}

// splitExponent separates the mantissa from a trailing e±nn.
func splitExponent(s string) (mant string, exp int32, err error) {
	i := strings.IndexAny(s, "eE")
	if i < 0 {
		return s, 0, nil
	}
	mant, tail := s[:i], s[i+1:]
	neg := false
	switch {
	case strings.HasPrefix(tail, "-"):
		neg, tail = true, tail[1:]
	case strings.HasPrefix(tail, "+"):
		tail = tail[1:]
	}
	if tail == "" || !allDigits(tail) || len(tail) > 5 {
		return "", 0, &ErrCast{From: KindText, To: KindNumeric, Val: s}
	}
	var n int32
	for _, c := range tail {
		n = n*10 + (c - '0')
	}
	if neg {
		n = -n
	}
	return mant, n, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ErrDivideByZero reports division by zero. Like ErrCast it is a plain error;
// the caller attaches the SQLSTATE and the position.
type ErrDivideByZero struct{}

func (e *ErrDivideByZero) Error() string { return "division by zero" }

// Div returns a ÷ b at the scale PostgreSQL would choose.
//
// Exact division does not always terminate -- 1÷3 has no finite decimal form --
// so a result scale has to be picked, and PostgreSQL picks one that gives at
// least sixteen significant digits, never fewer places than either operand had:
//
//	1 / 3       0.33333333333333333333   twenty places, since the result is < 1
//	10 / 3      3.3333333333333333       sixteen, since one digit is spent left of the point
//	7.5 / 2.5   3.0000000000000000
//
// The rule is reproduced rather than approximated, including its granularity:
// PostgreSQL works in base-10000 digits, so the number of places it adds moves
// in steps of four. A rule of one's own would be tidier and would disagree with
// every existing query.
func (d *Decimal) Div(b *Decimal) (*Decimal, error) {
	if b.IsZero() {
		return nil, &ErrDivideByZero{}
	}
	if d.IsZero() {
		return &Decimal{Unscaled: new(big.Int), Scale: max(d.Scale, b.Scale)}, nil
	}

	rscale := divScale(d, b)

	// Compute with one extra digit and round it off, which is how the last
	// place ends up correctly rounded rather than truncated.
	num := new(big.Int).Mul(d.Unscaled, pow10(rscale-d.Scale+b.Scale+1))
	q := new(big.Int).Quo(num, b.Unscaled)
	return (&Decimal{Unscaled: q, Scale: rscale + 1}).Round(rscale), nil
}

// divScale reproduces PostgreSQL's select_div_scale.
func divScale(a, b *Decimal) int32 {
	const (
		minSigDigits = 16   // NUMERIC_MIN_SIG_DIGITS
		decDigits    = 4    // DEC_DIGITS: PostgreSQL stores base-10000 digits
		maxScale     = 1000 // NUMERIC_MAX_DISPLAY_SCALE
	)

	qweight := nbaseWeight(a) - nbaseWeight(b)
	// PostgreSQL nudges the estimate down when the leading base-10000 digit of
	// the dividend is the smaller, because the quotient then has one fewer
	// digit than the weights alone suggest. This is what makes 1/3 twenty
	// places and 10/3 sixteen.
	if lead(a).Cmp(lead(b)) < 0 {
		qweight--
	}

	rscale := int32(minSigDigits) - qweight*decDigits
	rscale = max(rscale, a.Scale, b.Scale, 0)
	return min(rscale, maxScale)
}

// nbaseWeight is the base-10000 exponent of a decimal's leading digit group,
// which is PostgreSQL's `weight`.
func nbaseWeight(d *Decimal) int32 {
	if d.IsZero() {
		return 0
	}
	// Decimal exponent of the leading digit: for 250.0 that is 2.
	e := int32(len(new(big.Int).Abs(d.Unscaled).String())) - 1 - d.Scale
	// Floor division by four, which for a negative exponent is not truncation.
	if e >= 0 {
		return e / 4
	}
	return -((-e + 3) / 4)
}

// lead is the leading base-10000 digit, the value PostgreSQL compares when it
// adjusts the estimate above.
func lead(d *Decimal) *big.Int {
	if d.IsZero() {
		return new(big.Int)
	}
	abs := new(big.Int).Abs(d.Unscaled)
	// Shift so the value is an integer, then divide down to the leading group.
	w := nbaseWeight(d)
	// abs represents |d| × 10^scale; the leading group is |d| ÷ 10000^w.
	// Combine both shifts into a single power of ten to keep it exact.
	shift := d.Scale + 4*w
	if shift > 0 {
		return new(big.Int).Quo(abs, pow10(shift))
	}
	return new(big.Int).Mul(abs, pow10(-shift))
}
