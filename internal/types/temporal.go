package types

import (
	"strings"
	"time"
)

// The layouts accepted for a date, time or timestamp written as text.
//
// PostgreSQL accepts a famously large set of date styles; this is the ISO 8601
// subset, which is what every driver, ORM and migration tool emits and what the
// engine itself renders. Accepting more would mean choosing between the
// American and European readings of 01/02/2024, and there is no reading that is
// right for both.
//
// Order matters only in that the longest match wins: parsing tries each in turn
// and takes the first that consumes the whole string, so a layout that is a
// prefix of another must come after it.
var (
	dateLayouts = []string{
		"2006-01-02",
	}
	timeLayouts = []string{
		"15:04:05.999999999",
		"15:04:05",
		"15:04",
	}
	// A timestamp is a date and a time joined by either a space or a T. Both
	// spellings are in the wild -- SQL writes the space, JSON and RFC 3339 write
	// the T -- and a migration file routinely contains both.
	stampLayouts = []string{
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04",
		"2006-01-02",
	}
	// Zoned layouts are tried first for timestamptz. Go's reference layout uses
	// -07:00 for an offset and Z07:00 for one that may be the literal Z.
	zonedLayouts = []string{
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02T15:04:05.999999999-07",
		"2006-01-02 15:04:05-07",
		"2006-01-02T15:04:05-07",
	}
)

// ParseTemporal converts text to a date, time, timestamp or timestamptz.
//
// It reports ok false rather than an error, because the caller -- Cast -- owns
// the message and the SQLSTATE, and a failed conversion here is not exceptional:
// it is how `'abc'::date` is rejected.
func ParseTemporal(s string, want Kind) (Value, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Value{}, false
	}

	switch want {
	case KindDate:
		t, ok := tryLayouts(s, dateLayouts)
		if !ok {
			return Value{}, false
		}
		return Date(t.UTC().Unix() / 86400), true

	case KindTime:
		t, ok := tryLayouts(s, timeLayouts)
		if !ok {
			return Value{}, false
		}
		// The reference date parses to year zero, so the offset from midnight
		// has to be taken from the clock rather than from the instant.
		h, m, sec := t.Clock()
		micros := int64(h)*3600e6 + int64(m)*60e6 + int64(sec)*1e6 + int64(t.Nanosecond())/1000
		return Time(micros), true

	case KindTimestamp:
		// A zone offset is accepted and then discarded, because that is what
		// "without time zone" means: PostgreSQL takes the wall-clock reading and
		// ignores the offset rather than converting to UTC. Silently shifting
		// the value would make the same literal mean two different instants
		// depending on the column it landed in.
		if t, ok := tryLayouts(s, zonedLayouts); ok {
			y, mo, d := t.Date()
			h, mi, sec := t.Clock()
			naive := time.Date(y, mo, d, h, mi, sec, t.Nanosecond(), time.UTC)
			return Timestamp(naive.UnixMicro()), true
		}
		t, ok := tryLayouts(s, stampLayouts)
		if !ok {
			return Value{}, false
		}
		return Timestamp(t.UnixMicro()), true

	case KindTimestamptz:
		// Zoned first, so an explicit offset is honoured. Without one the
		// reading is taken as UTC, which is what a database with no session
		// time zone can do.
		if t, ok := tryLayouts(s, zonedLayouts); ok {
			return Timestamptz(t.UTC().UnixMicro()), true
		}
		t, ok := tryLayouts(s, stampLayouts)
		if !ok {
			return Value{}, false
		}
		return Timestamptz(t.UnixMicro()), true
	}
	return Value{}, false
}

// tryLayouts returns the first layout that parses the whole string.
//
// The result keeps whatever zone was written rather than being normalised here.
// That is load-bearing: a timestamp without time zone needs the wall-clock
// reading as written, and converting to UTC in this helper turned
// `12:30:00+02:00` into 10:30 for a column whose whole point is that it has no
// zone. Each caller converts, or does not, according to its own type.
//
// time.Parse already refuses trailing input, so a partial match cannot slip
// through as a success.
func tryLayouts(s string, layouts []string) (time.Time, bool) {
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
