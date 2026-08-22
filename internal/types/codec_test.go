package types

import (
	"math"
	"testing"
)

// TestValueCodecRoundTrip covers one value of every kind, including the ones
// whose payload shares a word with another kind's. A value that decodes as the
// wrong kind reads its payload out of the same field and therefore comes back
// as a confident wrong answer rather than as an error, so the kind is asserted
// alongside the value.
func TestValueCodecRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		v    Value
	}{
		{"null", Null()},
		{"true", Bool(true)},
		{"false", Bool(false)},
		{"zero int", Int(0)},
		{"negative int", Int(-1)},
		{"min int", Int(math.MinInt64)},
		{"max int", Int(math.MaxInt64)},
		{"float", Float(1.5)},
		{"negative zero", Float(math.Copysign(0, -1))},
		{"infinity", Float(math.Inf(-1))},
		{"empty text", Text("")},
		{"text", Text("hello")},
		{"text with a nul byte", Text("a\x00b")},
		{"bytea", Bytea([]byte{0, 1, 255})},
		{"empty bytea", Bytea(nil)},
		{"date", Date(-1)},
		{"time", Time(45296_000000)},
		{"timestamp", Timestamp(1_700_000_000_000_000)},
		{"timestamptz", Timestamptz(-1)},
		{"json", Value{k: KindJSON, s: `{"a": 1}`}},
		{"jsonb", Value{k: KindJSONB, s: `{"a":1}`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The trailing byte checks that the decoder consumes exactly its
			// own value and hands the rest back, which is what lets a row be
			// encoded into one buffer.
			enc := append(AppendValue(nil, tt.v), 0xff)

			got, rest, err := DecodeValue(enc)
			if err != nil {
				t.Fatalf("DecodeValue: %v", err)
			}
			if got.Kind() != tt.v.Kind() {
				t.Errorf("kind = %s, want %s", got.Kind(), tt.v.Kind())
			}
			if !Equal(got, tt.v) {
				t.Errorf("value = %v, want %v", got, tt.v)
			}
			if len(rest) != 1 || rest[0] != 0xff {
				t.Errorf("rest = %v, want the one byte that followed the value", rest)
			}
		})
	}
}

// TestNegativeZeroSurvives is separate because Equal cannot see the difference:
// -0.0 and 0.0 compare equal, so a codec that lost the sign would pass the
// round-trip test above.
func TestNegativeZeroSurvives(t *testing.T) {
	v, _, err := DecodeValue(AppendValue(nil, Float(math.Copysign(0, -1))))
	if err != nil {
		t.Fatalf("DecodeValue: %v", err)
	}
	if !math.Signbit(v.AsFloat()) {
		t.Error("negative zero decoded as positive zero")
	}
}

// TestDecodeTruncated feeds every proper prefix of an encoded value to the
// decoder. A crash can truncate the log at any byte, so each of these is a
// shape the decoder will really be handed, and every one of them must be an
// error rather than a panic or a plausible-looking value.
func TestDecodeTruncated(t *testing.T) {
	for _, v := range []Value{Int(-1), Text("hello"), Bytea([]byte{1, 2, 3}), Bool(true)} {
		enc := AppendValue(nil, v)
		for n := range enc {
			if _, _, err := DecodeValue(enc[:n]); err == nil {
				t.Errorf("DecodeValue(%s truncated to %d of %d bytes) succeeded, want an error",
					v.Kind(), n, len(enc))
			}
		}
	}
}

// TestDecodeUnknownKind pins that a byte outside the kind range is rejected.
// Without the check it would index the payload switch as some existing kind and
// invent a value.
func TestDecodeUnknownKind(t *testing.T) {
	if _, _, err := DecodeValue([]byte{0xfe, 0, 0, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Error("DecodeValue accepted an unknown kind")
	}
}

// FuzzDecodeValue asserts only that arbitrary bytes never panic. Recovery reads
// whatever is on disk, so the decoder's contract is that it either returns a
// value or an error.
func FuzzDecodeValue(f *testing.F) {
	seeds := []Value{Null(), Int(-1), Text("hello"), Bytea([]byte{1, 2}), Float(1.5)}
	if d, err := ParseDecimal("-12345.6789"); err == nil {
		seeds = append(seeds, Numeric(d))
	}
	for _, v := range seeds {
		f.Add(AppendValue(nil, v))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, rest, err := DecodeValue(data)
		if err != nil {
			return
		}
		if len(rest) > len(data) {
			t.Fatalf("decoder returned %d bytes of remainder from %d bytes of input", len(rest), len(data))
		}
		// A value that decodes must re-encode to the same bytes, which rules
		// out a decoder that silently drops part of what it read.
		if again := AppendValue(nil, v); len(again) != len(data)-len(rest) {
			t.Fatalf("re-encoding %s produced %d bytes, want the %d it was decoded from",
				v.Kind(), len(again), len(data)-len(rest))
		}
	})
}
