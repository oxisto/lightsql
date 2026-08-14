package types

import (
	"hash/maphash"
	"testing"
)

func TestParseJSONBCanonicalises(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"key order is normalised", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"whitespace is dropped", "{ \"a\" :\n 1 }", `{"a":1}`},
		{"nested objects sort too", `{"b":{"z":1,"y":2}}`, `{"b":{"y":2,"z":1}}`},
		// A later duplicate wins, which is what encoding/json does on decode and
		// what PostgreSQL's jsonb does on input.
		{"duplicate keys collapse", `{"a":1,"a":2}`, `{"a":2}`},
		// Array order is data, not formatting, so it must survive.
		{"array order is preserved", `[3,1,2]`, `[3,1,2]`},
		{"scalars are documents", `42`, `42`},
		{"strings stay quoted", `"x"`, `"x"`},
		// A large integer must not round-trip through float64. Without
		// UseNumber this comes back as 1.2345678901234568e+18.
		{"big integers keep precision", `{"n":1234567890123456789}`, `{"n":1234567890123456789}`},
		// HTML escaping is a display concern and must not alter a stored value.
		{"angle brackets are not escaped", `{"h":"<a>&b"}`, `{"h":"<a>&b"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := ParseJSONB(tt.in)
			if err != nil {
				t.Fatalf("ParseJSONB(%q) = %v", tt.in, err)
			}
			if got := v.AsString(); got != tt.want {
				t.Errorf("ParseJSONB(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if v.Kind() != KindJSONB {
				t.Errorf("kind = %v, want jsonb", v.Kind())
			}
		})
	}
}

func TestParseJSONBRejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"unterminated object", `{"a":1`},
		{"bare word", `{not json}`},
		{"empty input", ``},
		// A second document after the first is trailing garbage. encoding/json
		// stops at the first value, so without the explicit check this would
		// silently store only {"a":1}.
		{"trailing document", `{"a":1} {"b":2}`},
		{"trailing garbage", `{"a":1} xyz`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if v, err := ParseJSONB(tt.in); err == nil {
				t.Errorf("ParseJSONB(%q) = %q, want an error", tt.in, v.AsString())
			}
		})
	}
}

// TestJSONBEqualityIsStructural is the payoff of canonicalising on input: two
// documents that differ only in spelling are one value, so they compare equal
// and — per the hashing invariant — hash equally too.
func TestJSONBEqualityIsStructural(t *testing.T) {
	a, err := ParseJSONB(`{"b":1, "a":[1,2]}`)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseJSONB(`{"a":[1,2],"b":1}`)
	if err != nil {
		t.Fatal(err)
	}

	if Compare(a, b) != 0 {
		t.Errorf("Compare(%q, %q) = %d, want 0", a.AsString(), b.AsString(), Compare(a, b))
	}

	seed := maphash.MakeSeed()
	sum := func(v Value) uint64 {
		var h maphash.Hash
		h.SetSeed(seed)
		v.Hash(&h)
		return h.Sum64()
	}
	if sum(a) != sum(b) {
		t.Errorf("equal values hash differently: %d vs %d", sum(a), sum(b))
	}

	// json, by contrast, keeps what it was given, so the same two documents are
	// distinct values. That difference is the reason both types exist.
	if Compare(JSON(`{"b":1, "a":[1,2]}`), JSON(`{"a":[1,2],"b":1}`)) == 0 {
		t.Error("json compared equal, but it must preserve the text as written")
	}
}

func TestJSONField(t *testing.T) {
	doc := JSONB(`{"s":"txt","n":1,"o":{"k":"v"},"arr":[10,20,30],"nul":null}`)

	tests := []struct {
		name     string
		doc      Value
		key      Value
		wantJSON string // "" means SQL NULL
		wantText string // "\x00" means SQL NULL
	}{
		// The whole difference between the two operators: -> leaves a string
		// quoted as JSON, ->> unwraps it.
		{"string member", doc, Text("s"), `"txt"`, "txt"},
		{"number member", doc, Text("n"), "1", "1"},
		{"object member", doc, Text("o"), `{"k":"v"}`, `{"k":"v"}`},
		// A JSON null is SQL NULL through ->>, but a JSON null document
		// through ->. PostgreSQL draws the line in the same place.
		{"json null member", doc, Text("nul"), "null", "\x00"},
		{"missing member", doc, Text("zzz"), "", "\x00"},
		// An integer key against an object matches nothing, rather than being
		// coerced into the string "0".
		{"integer key on object", doc, Int(0), "", "\x00"},

		{"array element", JSONB(`[10,20,30]`), Int(1), "20", "20"},
		{"negative index counts from the end", JSONB(`[10,20,30]`), Int(-1), "30", "30"},
		{"index past the end", JSONB(`[10,20,30]`), Int(5), "", "\x00"},
		{"negative index past the start", JSONB(`[10,20,30]`), Int(-4), "", "\x00"},
		{"text key on array", JSONB(`[10,20]`), Text("0"), "", "\x00"},

		// A scalar document is not a container, so any lookup into it is NULL.
		{"member of a scalar", JSONB(`42`), Text("a"), "", "\x00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := JSONField(tt.doc, tt.key)
			if tt.wantJSON == "" {
				if !got.IsNull() {
					t.Errorf("-> = %q, want NULL", got.AsString())
				}
			} else if got.IsNull() {
				t.Errorf("-> = NULL, want %q", tt.wantJSON)
			} else if got.AsString() != tt.wantJSON {
				t.Errorf("-> = %q, want %q", got.AsString(), tt.wantJSON)
			}

			text := JSONText(tt.doc, tt.key)
			if tt.wantText == "\x00" {
				if !text.IsNull() {
					t.Errorf("->> = %q, want NULL", text.AsString())
				}
			} else if text.IsNull() {
				t.Errorf("->> = NULL, want %q", tt.wantText)
			} else if text.AsString() != tt.wantText {
				t.Errorf("->> = %q, want %q", text.AsString(), tt.wantText)
			}
		})
	}
}

// TestJSONFieldKeepsKind matters because it is what lets doc -> 'a' -> 'b'
// chain: the intermediate result has to still be a document.
func TestJSONFieldKeepsKind(t *testing.T) {
	inner := JSONField(JSONB(`{"a":{"b":1}}`), Text("a"))
	if inner.Kind() != KindJSONB {
		t.Fatalf("kind = %v, want jsonb", inner.Kind())
	}
	if got := JSONText(inner, Text("b")); got.AsString() != "1" {
		t.Errorf("chained lookup = %q, want 1", got.AsString())
	}
	if got := JSONField(JSON(`{"a":{"b":1}}`), Text("a")); got.Kind() != KindJSON {
		t.Errorf("kind = %v, want json", got.Kind())
	}
}

func TestJSONContains(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want Bool3
	}{
		{"identical objects", `{"a":1}`, `{"a":1}`, True},
		{"subset of members", `{"a":1,"b":2}`, `{"a":1}`, True},
		{"the empty object is contained in every object", `{"a":1}`, `{}`, True},
		{"a superset is not contained", `{"a":1}`, `{"a":1,"b":2}`, False},
		{"a differing value is not contained", `{"a":1}`, `{"a":2}`, False},
		{"containment is recursive", `{"a":{"x":1,"y":2}}`, `{"a":{"x":1}}`, True},
		{"a nested mismatch fails", `{"a":{"x":1}}`, `{"a":{"x":2}}`, False},

		// Arrays ignore order and repetition, which is PostgreSQL's rule.
		{"array order does not matter", `[1,2,3]`, `[3,1]`, True},
		{"repetition does not matter", `[1,2]`, `[1,1,1]`, True},
		{"a missing element fails", `[1,2]`, `[3]`, False},
		// The one asymmetry PostgreSQL keeps: an array contains a bare scalar,
		// but a scalar does not contain a single-element array.
		{"an array contains a bare scalar", `[1,2]`, `1`, True},
		{"a scalar does not contain an array", `1`, `[1]`, False},

		{"equal scalars", `1`, `1`, True},
		{"1 contains 1.0, numerically", `1`, `1.0`, True},
		{"unequal scalars", `1`, `2`, False},
		// The bug a %v comparison would introduce: these must not be equal.
		{"a string does not equal a boolean", `"true"`, `true`, False},
		{"a string does not equal a number", `"1"`, `1`, False},
		{"null equals null", `null`, `null`, True},

		{"an object does not contain a scalar", `{"a":1}`, `1`, False},
		{"an object is not an array", `{"a":1}`, `[1]`, False},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := ParseJSONB(tt.a)
			if err != nil {
				t.Fatal(err)
			}
			b, err := ParseJSONB(tt.b)
			if err != nil {
				t.Fatal(err)
			}
			if got := JSONContains(a, b); got != tt.want {
				t.Errorf("%s @> %s = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestJSONContainsNonJSON checks that a non-document operand is UNKNOWN rather
// than false, so it propagates through three-valued logic instead of quietly
// filtering rows out.
func TestJSONContainsNonJSON(t *testing.T) {
	if got := JSONContains(Text(`{"a":1}`), JSONB(`{"a":1}`)); got != Unknown {
		t.Errorf("text @> jsonb = %v, want Unknown", got)
	}
	if got := JSONContains(JSONB(`{"a":1}`), Int(1)); got != Unknown {
		t.Errorf("jsonb @> int = %v, want Unknown", got)
	}
}

func TestCastToJSON(t *testing.T) {
	// Casting text to jsonb canonicalises; casting to json does not, but both
	// refuse input that cannot be read back.
	v, err := Cast(Text(`{"b":1,"a":2}`), KindJSONB)
	if err != nil {
		t.Fatal(err)
	}
	if v.AsString() != `{"a":2,"b":1}` {
		t.Errorf("text::jsonb = %q, want canonical form", v.AsString())
	}

	v, err = Cast(Text(`{"b":1, "a":2}`), KindJSON)
	if err != nil {
		t.Fatal(err)
	}
	if v.AsString() != `{"b":1, "a":2}` {
		t.Errorf("text::json = %q, want the text as written", v.AsString())
	}

	for _, kind := range []Kind{KindJSON, KindJSONB} {
		if _, err := Cast(Text(`{oops`), kind); err == nil {
			t.Errorf("cast of malformed text to %v succeeded", kind)
		}
	}

	// []byte is what a caller passing json.Marshal output or a
	// json.RawMessage argument actually hands the driver.
	if _, err := Cast(Bytea([]byte(`{"a":1}`)), KindJSONB); err != nil {
		t.Errorf("bytea::jsonb = %v", err)
	}
}
