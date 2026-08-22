package types

import (
	"errors"
	"strconv"
	"strings"

	"github.com/oxisto/lightsql/internal/pgerr"
)

// ErrCast reports a conversion that SQL does not permit. It is a plain error
// rather than a pgerr value because the caller is the one that knows where the
// value was written and which SQLSTATE its context calls for; it wraps this
// with both. Returning a pgerr here instead gets it wrapped twice, which reads
// as "ERROR: ERROR: ..." with the SQLSTATE printed at each end.
type ErrCast struct {
	From, To Kind
	Val      string
}

func (e *ErrCast) Error() string {
	return "invalid input syntax for type " + e.To.String() + ": " + strconv.Quote(e.Val)
}

// CastState reports the SQLSTATE PostgreSQL uses for a failed conversion, which
// depends on what was being converted to: a malformed date is a datetime format
// error and everything else is an invalid text representation.
//
// It lives here rather than at the call sites because the binder and the
// executor both wrap conversion failures, and the two must not disagree about
// the code -- which is the kind of difference nobody notices until a caller
// switches on it.
func CastState(err error) string {
	var e *ErrCast
	if !errors.As(err, &e) {
		return pgerr.InvalidTextForType
	}
	switch e.To {
	case KindDate, KindTime, KindTimestamp, KindTimestamptz:
		return pgerr.InvalidDatetimeFormat
	default:
		return pgerr.InvalidTextForType
	}
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
		switch v.Kind() {
		case KindText:
			switch strings.ToLower(strings.TrimSpace(v.AsString())) {
			case "t", "true", "yes", "on", "1":
				return Bool(true), nil
			case "f", "false", "no", "off", "0":
				return Bool(false), nil
			}
		case KindInt:
			// Zero is false and everything else is true, which is what
			// PostgreSQL's integer-to-boolean cast does. Only the integer
			// types have this cast there; a float or a numeric does not.
			return Bool(v.AsInt() != 0), nil
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
		case KindNumeric:
			// Rounded, not truncated: this is the conversion an assignment to
			// an integer column performs, and PostgreSQL rounds there.
			if i, ok := v.AsDecimal().Round(0).Int64(); ok {
				return Int(i), nil
			}
		}

	case KindFloat:
		switch v.Kind() {
		case KindInt:
			return Float(float64(v.AsInt())), nil
		case KindNumeric:
			return Float(v.AsDecimal().Float64()), nil
		case KindText:
			if f, err := strconv.ParseFloat(strings.TrimSpace(v.AsString()), 64); err == nil {
				return Float(f), nil
			}
		}

	case KindNumeric:
		switch v.Kind() {
		case KindInt:
			return Numeric(DecimalFromInt(v.AsInt())), nil
		case KindFloat:
			// Through the shortest text that round-trips the float, which is
			// what PostgreSQL does: the alternative is the exact binary value,
			// and 0.1::float8::numeric would come out as 0.1000000000000000055.
			d, err := ParseDecimal(strconv.FormatFloat(v.AsFloat(), 'g', -1, 64))
			if err != nil {
				break
			}
			return Numeric(d), nil
		case KindText:
			d, err := ParseDecimal(v.AsString())
			if err != nil {
				break
			}
			return Numeric(d), nil
		}

	case KindBytea:
		if v.Kind() == KindText {
			// Decoded, not relabelled: the text of a bytea literal spells the
			// bytes rather than being them. See ParseBytea.
			return ParseBytea(v.AsString())
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
		case KindText:
			if out, ok := ParseTemporal(v.AsString(), want); ok {
				return out, nil
			}
		}

	case KindTime:
		if v.Kind() == KindText {
			if out, ok := ParseTemporal(v.AsString(), want); ok {
				return out, nil
			}
		}

	case KindDate:
		switch v.Kind() {
		case KindText:
			if out, ok := ParseTemporal(v.AsString(), want); ok {
				return out, nil
			}
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

	case KindJSON:
		// json keeps what it was given, but still refuses malformed input:
		// storing text that cannot be read back is not a favour to anyone.
		switch v.Kind() {
		case KindText, KindBytea, KindJSONB:
			if err := ValidateJSON(v.AsString()); err != nil {
				return Value{}, &ErrCast{From: v.Kind(), To: want, Val: v.String()}
			}
			return JSON(v.AsString()), nil
		}

	case KindJSONB:
		switch v.Kind() {
		case KindText, KindBytea, KindJSON:
			// []byte arrives here whenever a caller passes json.RawMessage or
			// the output of json.Marshal as a query argument.
			out, err := ParseJSONB(v.AsString())
			if err != nil {
				return Value{}, &ErrCast{From: v.Kind(), To: want, Val: v.String()}
			}
			return out, nil
		}
	}

	return Value{}, &ErrCast{From: v.Kind(), To: want, Val: v.String()}
}
