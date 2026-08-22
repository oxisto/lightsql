package types

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// JSON returns a json value, storing the text exactly as given.
//
// PostgreSQL's json type keeps the input verbatim: whitespace, key order and
// duplicate keys all survive. That is the difference from jsonb, and it is why
// the two are separate types rather than one with a flag.
func JSON(text string) Value { return Value{k: KindJSON, s: text} }

// JSONB returns a jsonb value from already-canonical text. Use ParseJSONB to
// canonicalise untrusted input.
func JSONB(canonical string) Value { return Value{k: KindJSONB, s: canonical} }

// ParseJSONB validates and canonicalises JSON text into a jsonb value.
//
// jsonb is stored decomposed, so two documents that differ only in whitespace,
// key order or duplicate keys are the same value. Canonicalising on the way in
// means equality and hashing are text comparisons afterwards, rather than a
// parse per comparison.
func ParseJSONB(text string) (Value, error) {
	canonical, err := canonicalJSON(text)
	if err != nil {
		return Value{}, err
	}
	return Value{k: KindJSONB, s: canonical}, nil
}

// ValidateJSON reports whether text is well-formed JSON, without changing it.
func ValidateJSON(text string) error {
	if !json.Valid([]byte(text)) {
		return &ErrJSON{Text: text}
	}
	return nil
}

// ErrJSON reports malformed JSON. Like ErrCast it is a plain sentinel, because
// types must not depend on pgerr; callers attach the SQLSTATE and position.
type ErrJSON struct {
	Text   string
	Detail string
}

func (e *ErrJSON) Error() string {
	if e.Detail != "" {
		return "invalid input syntax for type json: " + e.Detail
	}
	return "invalid input syntax for type json"
}

// canonicalJSON reparses and re-emits JSON in a stable form.
//
// Two details make this work. Decoding into any with UseNumber keeps a numeric
// literal as its original text, so a large integer does not silently round-trip
// through float64 and lose precision. And encoding a map sorts its keys, which
// is what gives two documents written in different key orders the same bytes.
func canonicalJSON(text string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return "", &ErrJSON{Text: text, Detail: err.Error()}
	}
	// Anything after the first value is trailing garbage, not a second
	// document: `{"a":1} {"b":2}` is not valid input.
	//
	// This asks for the next token and requires EOF, rather than asking More().
	// More() reports whether another element follows in the current array or
	// object, so it answers false for a stray closing brace — which let
	// `{"a":1}}` through and silently truncated it to `{"a":1}`. Requiring EOF
	// has no such blind spot.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", &ErrJSON{Text: text, Detail: "unexpected trailing data"}
	}

	var b strings.Builder
	if err := writeCanonical(&b, v); err != nil {
		return "", &ErrJSON{Text: text, Detail: err.Error()}
	}
	return b.String(), nil
}

// writeCanonical emits a decoded document the way PostgreSQL emits jsonb.
//
// The standard library's encoder is close but not the same, and the three
// differences are all visible to anyone who casts a jsonb to text:
//
//   - PostgreSQL puts a space after each colon and comma.
//   - It orders object keys by length first and then bytewise, so a, z, bb,
//     ccc rather than a, bb, ccc, z. Sorting only bytewise looks equally
//     canonical and is a different order.
//   - It reads a number as numeric and re-emits it, so 1e2 becomes 100 while
//     1.50 stays 1.50. The exponent is not part of the stored value.
func writeCanonical(b *strings.Builder, v any) error {
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(canonicalNumber(string(v)))
	case string:
		return writeJSONString(b, v)
	case []any:
		b.WriteByte('[')
		for i, e := range v {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := slices.SortedFunc(maps.Keys(v), compareJSONKeys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := writeJSONString(b, k); err != nil {
				return err
			}
			b.WriteString(": ")
			if err := writeCanonical(b, v[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unexpected %T in a decoded document", v)
	}
	return nil
}

// compareJSONKeys orders object keys as PostgreSQL does: shorter first, and
// bytewise within a length.
func compareJSONKeys(a, b string) int {
	if d := cmp.Compare(len(a), len(b)); d != 0 {
		return d
	}
	return cmp.Compare(a, b)
}

// canonicalNumber re-renders a JSON number the way PostgreSQL does, which is as
// a numeric: the exponent is folded in and trailing zeros are kept, so 1e2 is
// 100 and 1.50 stays 1.50.
//
// Text that will not parse as a decimal is left alone rather than dropped. The
// JSON decoder has already accepted it as a number, so refusing it here would
// reject a document over a disagreement between two parsers.
func canonicalNumber(text string) string {
	d, err := ParseDecimal(text)
	if err != nil {
		return text
	}
	return d.String()
}

// writeJSONString quotes a string, escaping only what JSON requires.
//
// HTML escaping would turn < and & into \u003c and \u0026, which is a display
// concern that has no business changing a stored value.
func writeJSONString(b *strings.Builder, s string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	// Encode appends a newline.
	b.WriteString(strings.TrimSuffix(buf.String(), "\n"))
	return nil
}

// JSONField implements the -> operator: the member or element at key, as JSON.
//
// A missing key, or an index off the end, yields SQL NULL rather than an error,
// which is what makes chained access such as doc -> 'a' -> 'b' usable without
// checking each step.
func JSONField(v, key Value) Value {
	field, ok := jsonLookup(v, key)
	if !ok {
		return Null()
	}
	return encodeJSON(v.Kind(), field)
}

// JSONText implements the ->> operator: the member or element at key, as text.
//
// The difference from -> is only in how a string is rendered: ->> gives the
// string itself, while -> gives it still quoted as JSON. Everything else is the
// same, which is why both share one lookup.
func JSONText(v, key Value) Value {
	field, ok := jsonLookup(v, key)
	if !ok {
		return Null()
	}
	switch t := field.(type) {
	case nil:
		// A JSON null becomes SQL NULL, not the four characters "null".
		return Null()
	case string:
		return Text(t)
	default:
		// Everything else keeps its JSON rendering, so an object or array comes
		// back as the text of the document rather than Go's formatting of it.
		return Text(encodeJSON(KindText, field).AsString())
	}
}

// jsonLookup finds the member or element at key. The second result is false when
// the document is not JSON, the key is the wrong type for the container, or the
// member is absent — all of which yield SQL NULL rather than an error, which is
// what makes chained access such as doc -> 'a' -> 'b' usable without checking
// each step.
func jsonLookup(v, key Value) (any, bool) {
	doc, ok := decodeJSON(v)
	if !ok {
		return nil, false
	}

	switch container := doc.(type) {
	case map[string]any:
		if key.Kind() != KindText {
			return nil, false
		}
		field, ok := container[key.AsString()]
		return field, ok

	case []any:
		if key.Kind() != KindInt {
			return nil, false
		}
		i := int(key.AsInt())
		// A negative index counts from the end, as PostgreSQL does.
		if i < 0 {
			i += len(container)
		}
		if i < 0 || i >= len(container) {
			return nil, false
		}
		return container[i], true

	default:
		return nil, false
	}
}

// JSONContains implements the @> operator: whether a contains b.
//
// Containment is structural and recursive, not textual. An object contains
// another when it has every one of its members; an array contains another when
// every element of the second appears in the first. A scalar contains only an
// equal scalar.
func JSONContains(a, b Value) Bool3 {
	da, ok := decodeJSON(a)
	if !ok {
		return Unknown
	}
	db, ok := decodeJSON(b)
	if !ok {
		return Unknown
	}
	return Bool3Of(jsonContains(da, db))
}

func jsonContains(a, b any) bool {
	switch bv := b.(type) {
	case map[string]any:
		av, ok := a.(map[string]any)
		if !ok {
			return false
		}
		for k, want := range bv {
			got, ok := av[k]
			if !ok || !jsonContains(got, want) {
				return false
			}
		}
		return true

	case []any:
		av, ok := a.([]any)
		if !ok {
			return false
		}
		// Every element of b must appear somewhere in a. Order and repetition
		// do not matter, which is PostgreSQL's rule.
		for _, want := range bv {
			found := false
			for _, got := range av {
				if jsonContains(got, want) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true

	default:
		// A scalar on the right matches an equal scalar on the left. An array
		// on the left also contains a bare scalar, which is the one asymmetry
		// PostgreSQL keeps for convenience.
		if av, ok := a.([]any); ok {
			for _, got := range av {
				if jsonEqual(got, b) {
					return true
				}
			}
			return false
		}
		return jsonEqual(a, b)
	}
}

// jsonEqual compares two decoded JSON scalars.
//
// It switches on the type rather than formatting both sides and comparing the
// text. Formatting would make the string "true" equal the boolean true, and the
// string "1" equal the number 1, which is exactly the collision that a %v-based
// comparison introduces and that nothing downstream would ever notice.
func jsonEqual(a, b any) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case json.Number:
		bv, ok := b.(json.Number)
		if !ok {
			return false
		}
		// Compare numerically rather than textually, so 1 and 1.0 match. The
		// textual fallback covers values too large for a float64, where the
		// literal text is the only exact representation left.
		af, aerr := av.Float64()
		bf, berr := bv.Float64()
		if aerr == nil && berr == nil {
			return af == bf
		}
		return av.String() == bv.String()
	default:
		// Objects and arrays never reach here: jsonContains handles them
		// structurally before falling through to a scalar comparison.
		return false
	}
}

func decodeJSON(v Value) (any, bool) {
	if v.Kind() != KindJSON && v.Kind() != KindJSONB {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(v.s))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, false
	}
	return out, true
}

// encodeJSON re-serialises a decoded fragment, preserving the kind it came from
// so that chained access keeps its type.
func encodeJSON(kind Kind, v any) Value {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return Null()
	}
	return Value{k: kind, s: strings.TrimSuffix(buf.String(), "\n")}
}
