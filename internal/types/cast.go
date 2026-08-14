package types

import (
	"strconv"
	"strings"
)

// ErrCast reports a conversion that SQL does not permit. It is a plain sentinel
// rather than a pgerr value because types must not depend on pgerr; callers
// wrap it with the SQLSTATE and position appropriate to their context.
type ErrCast struct {
	From, To Kind
	Val      string
}

func (e *ErrCast) Error() string {
	return "invalid input syntax for type " + e.To.String() + ": " + strconv.Quote(e.Val)
}

// Cast converts a value to the requested kind, applying only the conversions
// PostgreSQL performs implicitly.
//
// It is deliberately narrow. A permissive conversion here is how a float
// silently truncates to an integer, or an integer becomes a one-character
// string — the two failure modes of using reflect.Value.Convert, which will
// happily do both.
//
// Cast is used in two places that must agree: the binder folds constants at
// plan time, and the executor converts an argument whose declared parameter type
// was inferred from context. Sharing one implementation is what keeps a literal
// and a placeholder behaving identically.
func Cast(v Value, want Kind) (Value, error) {
	if v.IsNull() || v.Kind() == want {
		return v, nil
	}

	switch want {
	case KindText:
		return Text(v.String()), nil

	case KindBool:
		if v.Kind() == KindText {
			switch strings.ToLower(strings.TrimSpace(v.AsString())) {
			case "t", "true", "yes", "on", "1":
				return Bool(true), nil
			case "f", "false", "no", "off", "0":
				return Bool(false), nil
			}
		}

	case KindInt:
		switch v.Kind() {
		case KindBool:
			if v.AsBool() {
				return Int(1), nil
			}
			return Int(0), nil
		case KindFloat:
			// Only an exact integer converts; rounding silently would make
			// arithmetic depend on where a cast happened to be inserted.
			f := v.AsFloat()
			if i := int64(f); float64(i) == f {
				return Int(i), nil
			}
		case KindText:
			if i, err := strconv.ParseInt(strings.TrimSpace(v.AsString()), 10, 64); err == nil {
				return Int(i), nil
			}
		}

	case KindFloat:
		switch v.Kind() {
		case KindInt:
			return Float(float64(v.AsInt())), nil
		case KindText:
			if f, err := strconv.ParseFloat(strings.TrimSpace(v.AsString()), 64); err == nil {
				return Float(f), nil
			}
		}

	case KindBytea:
		if v.Kind() == KindText {
			return Value{k: KindBytea, s: v.AsString()}, nil
		}

	case KindTimestamp, KindTimestamptz:
		// timestamp and timestamptz share a payload — microseconds since the
		// epoch — and differ only in how they are rendered, so converting
		// between them relabels rather than shifts. This is the conversion a
		// time.Time argument needs, since the driver has no way to tell which
		// of the two a caller meant.
		switch v.Kind() {
		case KindTimestamp, KindTimestamptz:
			return Value{k: want, n: v.n}, nil
		case KindDate:
			return Value{k: want, n: uint64(v.AsInt() * 86400 * 1e6)}, nil
		}

	case KindDate:
		switch v.Kind() {
		case KindTimestamp, KindTimestamptz:
			// Truncating to the day must floor, not divide toward zero, or
			// dates before 1970 would land a day late.
			micros := v.AsInt()
			const perDay = 86400 * 1e6
			days := micros / perDay
			if micros < 0 && micros%perDay != 0 {
				days--
			}
			return Date(days), nil
		}

	}

	return Value{}, &ErrCast{From: v.Kind(), To: want, Val: v.String()}
}
