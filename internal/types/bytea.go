package types

import (
	"encoding/hex"
	"strings"
)

// ErrBytea reports text that is not a bytea literal. Like ErrCast it is a plain
// error rather than a pgerr value, because the caller is the one that knows the
// position to attach and the SQLSTATE the context calls for -- returning one
// here produced an error wrapped twice, reading "ERROR: ERROR: ..." with the
// SQLSTATE printed at both ends.
type ErrBytea struct{ Reason string }

func (e *ErrBytea) Error() string {
	return "invalid input syntax for type bytea: " + e.Reason
}

// A bytea is written as a string literal, which means the text has to be
// decoded rather than relabelled. Treating '\x0102' as the six characters that
// spell it is not a rounding error: the column then holds three times the bytes
// it was meant to, comparisons against a real byte string never match, and
// length reports 6. Nothing fails, which is what makes it worth being careful
// about.

// ParseBytea decodes the text of a bytea literal.
//
// Both of PostgreSQL's input formats are accepted, told apart the way
// PostgreSQL tells them apart: a leading \x means the rest is hex, and anything
// else is the older escape format, where a backslash introduces either another
// backslash or a three-digit octal byte and every other character stands for
// itself. That is why 'abc' is three bytes rather than an error.
func ParseBytea(s string) (Value, error) {
	if rest, ok := strings.CutPrefix(s, `\x`); ok {
		return parseHex(rest)
	}
	return parseEscape(s)
}

func parseHex(s string) (Value, error) {
	// PostgreSQL allows whitespace between digit pairs, so that a long value
	// can be wrapped across lines.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		b.WriteRune(r)
	}
	digits := b.String()

	if len(digits)%2 != 0 {
		return Value{}, &ErrBytea{Reason: "hexadecimal data has an odd number of digits"}
	}
	out, err := hex.DecodeString(digits)
	if err != nil {
		return Value{}, &ErrBytea{Reason: "invalid hexadecimal digit"}
	}
	return Bytea(out), nil
}

func parseEscape(s string) (Value, error) {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out = append(out, s[i])
			continue
		}
		i++
		if i >= len(s) {
			return Value{}, &ErrBytea{Reason: "a backslash must be doubled"}
		}
		if s[i] == '\\' {
			out = append(out, '\\')
			continue
		}
		// A backslash otherwise introduces exactly three octal digits. Fewer is
		// an error rather than a shorter number, because \1 followed by a digit
		// would otherwise be ambiguous.
		if i+2 >= len(s) || !octal(s[i]) || !octal(s[i+1]) || !octal(s[i+2]) {
			return Value{}, &ErrBytea{
				Reason: "a backslash must be followed by another backslash or three octal digits",
			}
		}
		n := (int(s[i]-'0') << 6) | (int(s[i+1]-'0') << 3) | int(s[i+2]-'0')
		if n > 0xff {
			return Value{}, &ErrBytea{Reason: "octal value is out of range"}
		}
		out = append(out, byte(n))
		i += 2
	}
	return Bytea(out), nil
}

func octal(c byte) bool { return c >= '0' && c <= '7' }

// EncodeBytea renders a byte string the way PostgreSQL renders one, which since
// version 9.0 is the hex form. It is the inverse of ParseBytea, so a value
// written out and read back is the value it started as.
func EncodeBytea(b string) string { return `\x` + hex.EncodeToString([]byte(b)) }
