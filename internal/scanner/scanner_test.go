package scanner

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/token"
)

// tok is a compact literal for the expected token stream. Val is only compared
// when non-empty for kinds that carry one, keeping the table readable.
type tok struct {
	kind token.Kind
	val  string
	ord  int
}

func TestScan(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []tok
	}{
		{
			name: "keywords fold and identifiers fold",
			src:  `SELECT Name FROM Users`,
			want: []tok{
				{kind: token.Select},
				{kind: token.Ident, val: "name"},
				{kind: token.From},
				{kind: token.Ident, val: "users"},
			},
		},
		{
			// ramsql matches keywords with a peek-ahead hack because its matcher
			// list tries COUNT before it has scanned the whole word.
			name: "keyword prefix of an identifier is an identifier",
			src:  `country counts selected`,
			want: []tok{
				{kind: token.Ident, val: "country"},
				{kind: token.Ident, val: "counts"},
				{kind: token.Ident, val: "selected"},
			},
		},
		{
			// The bug this pins: folding the sign into the literal makes a-1
			// and a - 1 parse differently.
			name: "minus is never folded into a numeric literal",
			src:  `a-1`,
			want: []tok{
				{kind: token.Ident, val: "a"},
				{kind: token.Minus},
				{kind: token.Number, val: "1"},
			},
		},
		{
			name: "spaced minus scans identically",
			src:  `a - 1`,
			want: []tok{
				{kind: token.Ident, val: "a"},
				{kind: token.Minus},
				{kind: token.Number, val: "1"},
			},
		},
		{
			name: "numeric forms",
			src:  `0 42 3.14 1. .5 1e10 1E+10 2e-3`,
			want: []tok{
				{kind: token.Number, val: "0"},
				{kind: token.Number, val: "42"},
				{kind: token.Number, val: "3.14"},
				{kind: token.Number, val: "1."},
				{kind: token.Number, val: ".5"},
				{kind: token.Number, val: "1e10"},
				{kind: token.Number, val: "1E+10"},
				{kind: token.Number, val: "2e-3"},
			},
		},
		{
			name: "qualified name is not a decimal",
			src:  `tbl.col`,
			want: []tok{
				{kind: token.Ident, val: "tbl"},
				{kind: token.Dot},
				{kind: token.Ident, val: "col"},
			},
		},
		{
			// Quote characters must not reach the parser, and '' is one quote.
			name: "string literal with doubled quote",
			src:  `'it''s here'`,
			want: []tok{{kind: token.String, val: "it's here"}},
		},
		{
			name: "empty string literal",
			src:  `''`,
			want: []tok{{kind: token.String, val: ""}},
		},
		{
			name: "escape string literal",
			src:  `E'line\nbreak\\done'`,
			want: []tok{{kind: token.String, val: "line\nbreak\\done"}},
		},
		{
			name: "quoted identifier keeps case and is not a string",
			src:  `"User"."Name"`,
			want: []tok{
				{kind: token.QuotedIdent, val: "User"},
				{kind: token.Dot},
				{kind: token.QuotedIdent, val: "Name"},
			},
		},
		{
			name: "quoted identifier with doubled quote",
			src:  `"a""b"`,
			want: []tok{{kind: token.QuotedIdent, val: `a"b`}},
		},
		{
			// ramsql sniffs dollar-quoted content and turns $$123$$ into a number.
			name: "dollar quoted content is never type-sniffed",
			src:  `$$123$$`,
			want: []tok{{kind: token.String, val: "123"}},
		},
		{
			name: "tagged dollar quoting nests other dollars",
			src:  `$tag$ contains $$ and 'quotes' $tag$`,
			want: []tok{{kind: token.String, val: ` contains $$ and 'quotes' `}},
		},
		{
			name: "positional parameters carry their own ordinal",
			src:  `$1 $12 $2`,
			want: []tok{
				{kind: token.Param, ord: 1},
				{kind: token.Param, ord: 12},
				{kind: token.Param, ord: 2},
			},
		},
		{
			// Ordinals are assigned here rather than by a counter mutated during
			// a later tree walk.
			name: "question marks are numbered at scan time",
			src:  `? ? ?`,
			want: []tok{
				{kind: token.Param, ord: 1},
				{kind: token.Param, ord: 2},
				{kind: token.Param, ord: 3},
			},
		},
		{
			name: "question mark inside a string is not a parameter",
			src:  `'? $1'`,
			want: []tok{{kind: token.String, val: "? $1"}},
		},
		{
			name: "line comment",
			src:  "a -- ignored 'text'\nb",
			want: []tok{
				{kind: token.Ident, val: "a"},
				{kind: token.Ident, val: "b"},
			},
		},
		{
			name: "nested block comment",
			src:  `a /* outer /* inner */ still */ b`,
			want: []tok{
				{kind: token.Ident, val: "a"},
				{kind: token.Ident, val: "b"},
			},
		},
		{
			name: "operators including two-character forms",
			src:  `:: || <> != <= >= < > = + - * / % ^ ( ) , ; .`,
			want: []tok{
				{kind: token.DoubleColon}, {kind: token.Concat},
				{kind: token.NotEq}, {kind: token.NotEq},
				{kind: token.LessEq}, {kind: token.GreaterEq},
				{kind: token.Less}, {kind: token.Greater}, {kind: token.Eq},
				{kind: token.Plus}, {kind: token.Minus}, {kind: token.Star},
				{kind: token.Slash}, {kind: token.Percent}, {kind: token.Caret},
				{kind: token.LParen}, {kind: token.RParen}, {kind: token.Comma},
				{kind: token.Semicolon}, {kind: token.Dot},
			},
		},
		{
			name: "cast operator is not two colons",
			src:  `x::int`,
			want: []tok{
				{kind: token.Ident, val: "x"},
				{kind: token.DoubleColon},
				{kind: token.Ident, val: "int"},
			},
		},
		{
			name: "non-ascii identifier",
			src:  `Straße`,
			want: []tok{{kind: token.Ident, val: "straße"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Tokens(tt.src)
			if err != nil {
				t.Fatalf("Tokens(%q) returned error: %v", tt.src, err)
			}
			// Drop the trailing EOF, which every stream has.
			got = got[:len(got)-1]
			if len(got) != len(tt.want) {
				t.Fatalf("got %d tokens, want %d\ngot:  %s\nwant: %s",
					len(got), len(tt.want), formatGot(got), formatWant(tt.want))
			}
			for i, w := range tt.want {
				g := got[i]
				if g.Kind != w.kind {
					t.Errorf("token %d: kind = %s, want %s", i, g.Kind, w.kind)
				}
				if w.val != "" && g.Val != w.val {
					t.Errorf("token %d: val = %q, want %q", i, g.Val, w.val)
				}
				if w.ord != 0 && g.Ord != w.ord {
					t.Errorf("token %d: ord = %d, want %d", i, g.Ord, w.ord)
				}
			}
		})
	}
}

// TestScanPositions checks that every token can be pointed at in the source,
// which is what makes positional error messages possible at all.
func TestScanPositions(t *testing.T) {
	src := `SELECT a FROM t WHERE b = 'x'`
	want := []token.Pos{0, 7, 9, 14, 16, 22, 24, 26}

	got, err := Tokens(src)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	got = got[:len(got)-1]
	if len(got) != len(want) {
		t.Fatalf("got %d tokens, want %d: %s", len(got), len(want), formatGot(got))
	}
	for i, w := range want {
		if got[i].Pos != w {
			t.Errorf("token %d (%s): pos = %d, want %d", i, got[i].Kind, got[i].Pos, w)
		}
	}
}

func TestScanErrors(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantPos token.Pos
		wantMsg string
	}{
		{"unterminated string", `SELECT 'abc`, 7, "unterminated quoted string"},
		{"unterminated quoted ident", `SELECT "abc`, 7, "unterminated quoted identifier"},
		{"zero length ident", `SELECT ""`, 7, "zero-length delimited identifier"},
		{"unterminated block comment", `SELECT /* x`, 7, "unterminated /* comment"},
		{"unterminated dollar quote", `SELECT $tag$abc`, 7, "unterminated dollar-quoted string"},
		{"trailing junk after number", `SELECT 1abc`, 8, "trailing junk after numeric literal"},
		{"unexpected character", `SELECT #`, 7, "unexpected character"},
		{"mixed placeholders dollar first", `SELECT $1, ?`, 11, "cannot mix"},
		{"mixed placeholders question first", `SELECT ?, $1`, 10, "cannot mix"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Tokens(tt.src)
			if err == nil {
				t.Fatalf("Tokens(%q) succeeded, want error", tt.src)
			}
			var e *pgerr.Error
			if !errors.As(err, &e) {
				t.Fatalf("error %v is not a *pgerr.Error", err)
			}
			if e.SQLState() != pgerr.SyntaxError {
				t.Errorf("SQLSTATE = %s, want %s", e.SQLState(), pgerr.SyntaxError)
			}
			if !strings.Contains(e.Message, tt.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", e.Message, tt.wantMsg)
			}
			if e.Pos != tt.wantPos {
				t.Errorf("position = %d, want %d (error: %v)", e.Pos, tt.wantPos, err)
			}
		})
	}
}

// TestScanEOFIsRepeatable ensures the parser can look ahead past the end without
// a bounds check.
func TestScanEOFIsRepeatable(t *testing.T) {
	s := New(`a`)
	for range 3 {
		if _, err := s.Scan(); err != nil {
			t.Fatalf("Scan: %v", err)
		}
	}
	tk, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if tk.Kind != token.EOF {
		t.Errorf("kind = %s, want EOF", tk.Kind)
	}
}

func FuzzScan(f *testing.F) {
	for _, s := range []string{
		`SELECT * FROM t WHERE a = $1`,
		`$tag$body$tag$`,
		`'it''s'`,
		`/* /* */ */`,
		`E'\n'`,
		`1e-3`,
	} {
		f.Add(s)
	}
	// The contract is only that scanning never panics and always terminates;
	// malformed input is expected to return an error.
	f.Fuzz(func(t *testing.T, src string) {
		_, _ = Tokens(src)
	})
}

func formatGot(ts []token.Token) string {
	s := make([]string, len(ts))
	for i, t := range ts {
		s[i] = format(t.Kind, t.Val)
	}
	return strings.Join(s, " ")
}

func formatWant(ts []tok) string {
	s := make([]string, len(ts))
	for i, t := range ts {
		s[i] = format(t.kind, t.val)
	}
	return strings.Join(s, " ")
}

func format(k token.Kind, val string) string {
	if val == "" {
		return k.String()
	}
	return fmt.Sprintf("%s(%s)", k, val)
}
