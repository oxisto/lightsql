package types

import (
	"bytes"
	"strings"
	"testing"
)

// TestParseBytea covers both input formats. The distinction that matters is
// that the text of a bytea literal *spells* the bytes rather than being them:
// relabelling '\x0102' as bytea stores six bytes where two were meant, and
// nothing fails -- comparisons simply never match and length reports 6.
func TestParseBytea(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []byte
	}{
		{"hex", `\x0102`, []byte{1, 2}},
		{"hex upper case digits", `\xDEADBEEF`, []byte{0xde, 0xad, 0xbe, 0xef}},
		{"hex empty", `\x`, nil},
		// PostgreSQL allows whitespace between pairs so a long value can wrap.
		{"hex with whitespace", "\\x01 02\n03", []byte{1, 2, 3}},

		// Anything not starting with \x is the older escape format, where most
		// characters stand for themselves. This is why 'abc' is three bytes
		// rather than an error.
		{"escape plain text", "abc", []byte("abc")},
		{"escape empty", "", nil},
		{"escape octal", `\001\002\377`, []byte{1, 2, 255}},
		{"escape doubled backslash", `\\`, []byte{'\\'}},
		{"escape mixed", `a\\b\101c`, []byte{'a', '\\', 'b', 'A', 'c'}},
		// A high byte is why bytea exists: it is not text and need not be
		// valid UTF-8.
		{"escape high bytes", `\376\377`, []byte{254, 255}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBytea(tt.in)
			if err != nil {
				t.Fatalf("ParseBytea(%q): %v", tt.in, err)
			}
			if got.Kind() != KindBytea {
				t.Fatalf("kind = %s, want bytea", got.Kind())
			}
			if !bytes.Equal(got.AsBytes(), tt.want) {
				t.Errorf("ParseBytea(%q) = %v, want %v", tt.in, got.AsBytes(), tt.want)
			}
		})
	}
}

// TestParseByteaErrors covers the input that must be refused. Accepting any of
// it would mean storing something other than what was written, silently.
func TestParseByteaErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"odd hex digits", `\x010`, "odd number"},
		{"not a hex digit", `\xZZ`, "hexadecimal digit"},
		{"trailing backslash", `abc\`, "doubled"},
		{"single octal digit", `\1`, "three octal digits"},
		{"too few octal digits", `\12`, "three octal digits"},
		{"not octal", `\999`, "three octal digits"},
		{"bare escape", `\n`, "three octal digits"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseBytea(tt.in)
			if err == nil {
				t.Fatalf("ParseBytea(%q) succeeded", tt.in)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestByteaRoundTrips pins that what is rendered can be read back. A value
// written into an error message, a CSV file or a cast to text has to be
// something this parser would accept again.
func TestByteaRoundTrips(t *testing.T) {
	for _, in := range [][]byte{nil, {0}, {1, 2}, {255, 254, 0, 1}, []byte("hello")} {
		v := Bytea(in)
		text := v.String()
		if !strings.HasPrefix(text, `\x`) {
			t.Errorf("Bytea(%v).String() = %q, want the hex form", in, text)
		}
		back, err := ParseBytea(text)
		if err != nil {
			t.Fatalf("reading back %q: %v", text, err)
		}
		if !bytes.Equal(back.AsBytes(), in) {
			t.Errorf("round trip of %v gave %v", in, back.AsBytes())
		}
	}
}

// TestByteaCastDecodes is the bug this was written for: casting text to bytea
// has to decode rather than relabel.
func TestByteaCastDecodes(t *testing.T) {
	got, err := Cast(Text(`\x0102`), KindBytea)
	if err != nil {
		t.Fatalf("Cast: %v", err)
	}
	if n := len(got.AsBytes()); n != 2 {
		t.Errorf("casting %q to bytea gave %d bytes, want 2", `\x0102`, n)
	}
}
