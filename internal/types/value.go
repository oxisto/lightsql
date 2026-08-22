// Package types defines lightsql's SQL type system: the runtime representation
// of a single column value, its three-valued comparison semantics, and the
// conversions to and from Go types at the driver boundary.
package types

import (
	"cmp"
	"hash/maphash"
	"math"
	"strconv"
	"time"
)

// Kind identifies which SQL type a Value holds.
type Kind uint8

const (
	// KindNull is the SQL NULL. It is a Kind rather than a nil interface so
	// that "is this NULL" is one comparison with no typed-nil trap, and so that
	// a NULL can never be mistaken for a zero value.
	KindNull Kind = iota
	KindBool
	KindInt   // int64: smallint, integer, bigint, serial
	KindFloat // float64: real, double precision
	KindText  // text, varchar, char
	KindBytea // []byte
	KindDate  // days since 1970-01-01
	KindTime  // microseconds since midnight
	KindTimestamp
	KindTimestamptz
	// KindJSON keeps the document text exactly as written; KindJSONB keeps it
	// canonicalised. Both live in the string payload, so neither grows Value.
	KindJSON
	KindJSONB
)

// kindNames are the canonical SQL spellings, so that both error messages and
// the type name reported for a computed column match what PostgreSQL says.
var kindNames = [...]string{
	KindNull:        "null",
	KindBool:        "boolean",
	KindInt:         "bigint",
	KindFloat:       "double precision",
	KindText:        "text",
	KindBytea:       "bytea",
	KindDate:        "date",
	KindTime:        "time without time zone",
	KindTimestamp:   "timestamp without time zone",
	KindTimestamptz: "timestamp with time zone",
	KindJSON:        "json",
	KindJSONB:       "jsonb",
}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// IsNumeric reports whether k is an arithmetic type, and therefore whether it
// participates in numeric promotion during comparison.
func (k Kind) IsNumeric() bool { return k == KindInt || k == KindFloat }

// Value is a single SQL value.
//
// It is a struct rather than an `any` so that storing an int or a timestamp does
// not allocate and box, NULL is unambiguous, and every operator dispatches on a
// small integer Kind instead of a reflect-based type switch. The layout is
// 32 bytes: the scalar payload shares one word, and only the string-shaped kinds
// use the string header.
type Value struct {
	k Kind
	// n holds the payload of the scalar kinds: an int64, the bits of a float64,
	// a bool, a day count, or a microsecond count — whichever k names.
	n uint64
	// s holds the payload of the string-shaped kinds: KindText and KindBytea.
	s string
}

// Null returns the SQL NULL value.
func Null() Value { return Value{k: KindNull} }

// Bool returns a boolean value.
func Bool(b bool) Value {
	var n uint64
	if b {
		n = 1
	}
	return Value{k: KindBool, n: n}
}

// Int returns a 64-bit integer value.
func Int(i int64) Value { return Value{k: KindInt, n: uint64(i)} }

// Float returns a double precision value.
func Float(f float64) Value { return Value{k: KindFloat, n: math.Float64bits(f)} }

// Text returns a text value.
func Text(s string) Value { return Value{k: KindText, s: s} }

// Bytea returns a byte string value. The bytes are copied into an immutable
// string, so the caller may reuse b.
func Bytea(b []byte) Value { return Value{k: KindBytea, s: string(b)} }

// Date returns a date, given as days since 1970-01-01.
func Date(days int64) Value { return Value{k: KindDate, n: uint64(days)} }

// Time returns a time of day, given as microseconds since midnight.
func Time(micros int64) Value { return Value{k: KindTime, n: uint64(micros)} }

// Timestamp returns a timestamp without time zone, as microseconds since the
// Unix epoch.
func Timestamp(micros int64) Value { return Value{k: KindTimestamp, n: uint64(micros)} }

// Timestamptz returns a timestamp with time zone, as microseconds since the
// Unix epoch. The zone itself is not stored: like PostgreSQL, the value is an
// absolute instant and is rendered in the session zone.
func Timestamptz(micros int64) Value { return Value{k: KindTimestamptz, n: uint64(micros)} }

// TimeValue returns a date, time or timestamp value from a time.Time, truncated
// to microseconds, which is PostgreSQL's resolution.
//
// The payload each kind expects is different -- days for a date, microseconds
// since midnight for a time, microseconds since the epoch for a timestamp -- and
// they all live in the same field. Writing microseconds into a date would not
// fail; it would produce a date some three billion years hence, which is exactly
// the kind of confident wrong answer the Value layout makes possible if the kind
// is not consulted.
func TimeValue(t time.Time, k Kind) Value {
	switch k {
	case KindDate:
		return Date(unixDay(t))
	case KindTime:
		return Time(t.UnixMicro() - unixDay(t)*microsPerDay)
	default:
		return Value{k: k, n: uint64(t.UnixMicro())}
	}
}

// microsPerDay is the length of a day in microseconds. Leap seconds do not
// appear in Unix time, so a day is always exactly this long.
const microsPerDay = 24 * 60 * 60 * 1e6

// unixDay returns the number of whole days between the epoch and t, rounding
// towards the past.
//
// Go's integer division truncates towards zero, which for a date before 1970
// would land on the following midnight and make the time of day negative. Every
// date in a test suite is after 1970 and would never show it.
func unixDay(t time.Time) int64 {
	micros := t.UnixMicro()
	days := micros / microsPerDay
	if micros < 0 && micros%microsPerDay != 0 {
		days--
	}
	return days
}

// Kind returns the value's type.
func (v Value) Kind() Kind { return v.k }

// IsNull reports whether v is SQL NULL.
func (v Value) IsNull() bool { return v.k == KindNull }

// AsBool returns the boolean payload. It is only meaningful for KindBool.
func (v Value) AsBool() bool { return v.n != 0 }

// AsInt returns the integer payload, meaningful for KindInt and the date/time
// kinds, whose payloads are also integer counts.
func (v Value) AsInt() int64 { return int64(v.n) }

// AsFloat returns the value as a float64, promoting an integer if needed.
func (v Value) AsFloat() float64 {
	if v.k == KindInt {
		return float64(int64(v.n))
	}
	return math.Float64frombits(v.n)
}

// AsString returns the text or bytea payload.
func (v Value) AsString() string { return v.s }

// AsBytes returns the bytea payload as a byte slice. The result is a copy.
func (v Value) AsBytes() []byte { return []byte(v.s) }

// AsTime reconstructs a time.Time from a date, time or timestamp value, in UTC.
func (v Value) AsTime() time.Time {
	switch v.k {
	case KindDate:
		return time.UnixMicro(int64(v.n) * 86400 * 1e6).UTC()
	case KindTime:
		return time.UnixMicro(int64(v.n)).UTC()
	default:
		return time.UnixMicro(int64(v.n)).UTC()
	}
}

// String renders the value in its SQL text representation. NULL renders as
// "NULL"; this is for diagnostics, not for output formatting.
func (v Value) String() string {
	switch v.k {
	case KindNull:
		return "NULL"
	case KindBool:
		return strconv.FormatBool(v.AsBool())
	case KindInt:
		return strconv.FormatInt(v.AsInt(), 10)
	case KindFloat:
		return strconv.FormatFloat(v.AsFloat(), 'g', -1, 64)
	case KindText, KindBytea, KindJSON, KindJSONB:
		return v.s
	case KindDate:
		return v.AsTime().Format("2006-01-02")
	case KindTime:
		return v.AsTime().Format("15:04:05.999999")
	default:
		return v.AsTime().Format("2006-01-02 15:04:05.999999")
	}
}

// Compare defines a total order over all values, including NULLs, and never
// fails. It is what indexes, ORDER BY and grouping use.
//
// This is deliberately distinct from the SQL comparison operators below: sorting
// needs every pair of values to be orderable, whereas `a < b` must yield UNKNOWN
// when either side is NULL. Conflating the two is how NULL handling goes wrong.
//
// NULLs sort last, matching PostgreSQL's default of NULLS LAST for ASC. Values
// of different kinds order by kind, which only arises in a heterogeneous
// context the binder would otherwise have rejected.
func Compare(a, b Value) int {
	if a.k == KindNull && b.k == KindNull {
		return 0
	}
	if a.k == KindNull {
		return 1 // NULL sorts after everything
	}
	if b.k == KindNull {
		return -1
	}
	if a.k != b.k {
		if a.k.IsNumeric() && b.k.IsNumeric() {
			return cmpFloat(a.AsFloat(), b.AsFloat())
		}
		return cmp.Compare(a.k, b.k)
	}
	switch a.k {
	case KindBool:
		return cmp.Compare(a.n, b.n)
	case KindFloat:
		return cmpFloat(a.AsFloat(), b.AsFloat())
	case KindText, KindBytea, KindJSON, KindJSONB:
		// jsonb is canonicalised on input, so comparing the text is comparing
		// the document. This is deterministic but not PostgreSQL's jsonb
		// ordering, which ranks by type before value; the matrix says so.
		return cmp.Compare(a.s, b.s)
	default:
		// KindInt and the date/time kinds are all signed integer counts.
		return cmp.Compare(a.AsInt(), b.AsInt())
	}
}

// Equal reports exact identity under the total order, for grouping and DISTINCT.
// Unlike SQL equality, two NULLs are equal here — which is precisely what
// GROUP BY and DISTINCT require.
func Equal(a, b Value) bool { return Compare(a, b) == 0 }

// Eq implements the SQL = operator, with three-valued logic.
func Eq(a, b Value) Bool3 { return sqlCmp(a, b, func(c int) bool { return c == 0 }) }

// Ne implements the SQL <> operator.
func Ne(a, b Value) Bool3 { return sqlCmp(a, b, func(c int) bool { return c != 0 }) }

// Lt implements the SQL < operator.
func Lt(a, b Value) Bool3 { return sqlCmp(a, b, func(c int) bool { return c < 0 }) }

// Le implements the SQL <= operator.
func Le(a, b Value) Bool3 { return sqlCmp(a, b, func(c int) bool { return c <= 0 }) }

// Gt implements the SQL > operator.
func Gt(a, b Value) Bool3 { return sqlCmp(a, b, func(c int) bool { return c > 0 }) }

// Ge implements the SQL >= operator.
func Ge(a, b Value) Bool3 { return sqlCmp(a, b, func(c int) bool { return c >= 0 }) }

// sqlCmp applies a comparison predicate under SQL semantics: any NULL operand
// makes the whole comparison UNKNOWN, regardless of the other operand.
func sqlCmp(a, b Value, pred func(int) bool) Bool3 {
	if a.k == KindNull || b.k == KindNull {
		return Unknown
	}
	return Bool3Of(pred(Compare(a, b)))
}

// IsDistinctFrom implements IS DISTINCT FROM, the NULL-aware inequality: it
// never returns UNKNOWN, and treats two NULLs as not distinct.
func IsDistinctFrom(a, b Value) Bool3 { return Bool3Of(!Equal(a, b)) }

// Truth interprets a value in a boolean context, as WHERE and ON do. A NULL is
// UNKNOWN, and anything non-boolean is a binder error that should never reach
// here, so it is treated as UNKNOWN rather than panicking mid-query.
func (v Value) Truth() Bool3 {
	switch v.k {
	case KindBool:
		return Bool3Of(v.AsBool())
	default:
		return Unknown
	}
}

// Hash writes v into h, for hash joins, hash aggregation and DISTINCT.
//
// The payload is written in full rather than via a formatted string, because
// hashing fmt output conflates int64(1), uint64(1) and the text "1".
//
// The invariant Hash must preserve is that values comparing equal hash equally,
// or a hash join silently drops matching rows. Since Compare promotes across the
// numeric kinds, Int(1) and Float(1) are equal and so must hash alike — which is
// why the tag mixed in is a hash class rather than the kind itself.
func (v Value) Hash(h *maphash.Hash) {
	h.WriteByte(v.k.hashClass())
	switch v.k {
	case KindNull:
	case KindText, KindBytea, KindJSON, KindJSONB:
		h.WriteString(v.s)
	case KindInt:
		writeUint64(h, v.n)
	case KindFloat:
		// Normalise a float holding an exact integer onto the integer payload,
		// completing the promotion above.
		f := v.AsFloat()
		if i := int64(f); float64(i) == f {
			writeUint64(h, uint64(i))
			return
		}
		writeUint64(h, v.n)
	default:
		writeUint64(h, v.n)
	}
}

// hashClass collapses kinds that Compare treats as mutually comparable onto one
// tag, so that equal values share a hash.
func (k Kind) hashClass() byte {
	if k.IsNumeric() {
		return byte(KindInt)
	}
	return byte(k)
}

func writeUint64(h *maphash.Hash, n uint64) {
	var b [8]byte
	for i := range b {
		b[i] = byte(n >> (8 * i))
	}
	h.Write(b[:])
}

// cmpFloat orders floats totally, which requires deciding where NaN goes.
// The other kinds use cmp.Compare directly; only floats need custom handling.
// PostgreSQL sorts NaN greater than every other float, including infinity.
func cmpFloat(a, b float64) int {
	aNaN, bNaN := math.IsNaN(a), math.IsNaN(b)
	switch {
	case aNaN && bNaN:
		return 0
	case aNaN:
		return 1
	case bNaN:
		return -1
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
