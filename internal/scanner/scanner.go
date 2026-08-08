// Package scanner turns SQL text into a stream of tokens.
//
// The scanner is a single left-to-right pass. Identifiers are scanned once and
// then looked up in a keyword map, rather than trying a list of per-keyword
// matchers in a load-bearing order — which is what makes COUNT/COUNTRY-style
// collisions and "does -1 lex as one token or two" into design questions rather
// than accidents.
//
// Two consequences are worth stating explicitly because the rest of the front
// end depends on them:
//
//   - Quote characters never reach the parser. A quoted string arrives as a
//     single token.String whose Val already has doubled-quote and backslash
//     escapes resolved, and a delimited identifier arrives as a single
//     token.QuotedIdent.
//   - A leading sign is never folded into a numeric literal. "a-1" and "a - 1"
//     produce the same three tokens, so arithmetic and negative literals cannot
//     disagree.
package scanner

import (
	"strings"
	"unicode/utf8"

	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/token"
)

// Scanner reads tokens from a SQL string.
type Scanner struct {
	src string
	pos int // byte offset of the next unread byte

	// Parameter placeholders come in two flavours and lightsql accepts either,
	// but not both in one statement: $N is self-numbering, while each ? takes
	// the next ordinal. Resolving ? here means nothing downstream needs a
	// mutable counter threaded through a tree walk.
	nextOrd    int
	usedDollar bool
	usedQM     bool
}

// New returns a Scanner over src.
func New(src string) *Scanner {
	return &Scanner{src: src, nextOrd: 1}
}

// Tokens scans src to completion and returns all tokens including the final EOF.
// It is the convenient entry point for tests and for the parser, which wants
// arbitrary lookahead over a statement.
func Tokens(src string) ([]token.Token, error) {
	s := New(src)
	var out []token.Token
	for {
		tok, err := s.Scan()
		if err != nil {
			return nil, err
		}
		out = append(out, tok)
		if tok.Kind == token.EOF {
			return out, nil
		}
	}
}

// Scan returns the next token. At the end of input it returns a token.EOF token
// repeatedly, so callers may look ahead without a bounds check.
func (s *Scanner) Scan() (token.Token, error) {
	if err := s.skipSpaceAndComments(); err != nil {
		return token.Token{}, err
	}
	if s.pos >= len(s.src) {
		return token.Token{Kind: token.EOF, Pos: token.Pos(s.pos)}, nil
	}

	start := s.pos
	c := s.src[s.pos]

	// E'...' is a PostgreSQL escape string, so the E is not an identifier. This
	// has to be tested before the identifier case would swallow it.
	if (c == 'E' || c == 'e') && s.peekAt(1) == '\'' {
		s.pos++
		return s.scanString(true)
	}

	switch {
	case isIdentStart(c):
		return s.scanIdent(), nil
	case isDigit(c):
		return s.scanNumber()
	case c == '.' && s.peekAt(1) != 0 && isDigit(s.peekAt(1)):
		// A qualified name can never have a digit after the dot, so ".5" is
		// unambiguously a numeric literal.
		return s.scanNumber()
	case c == '\'':
		return s.scanString(false)
	case c == '"':
		return s.scanQuotedIdent()
	case c == '$':
		return s.scanDollar()
	case c == '?':
		s.pos++
		return s.questionParam(token.Pos(start))
	}

	return s.scanOperator()
}

// skipSpaceAndComments advances past whitespace, -- line comments and nestable
// /* */ block comments.
func (s *Scanner) skipSpaceAndComments() error {
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			s.pos++
		case c == '-' && s.peekAt(1) == '-':
			s.pos += 2
			for s.pos < len(s.src) && s.src[s.pos] != '\n' {
				s.pos++
			}
		case c == '/' && s.peekAt(1) == '*':
			if err := s.skipBlockComment(); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

// skipBlockComment consumes a /* */ comment. PostgreSQL nests them, so that a
// block containing SQL that itself contains a comment can be commented out.
func (s *Scanner) skipBlockComment() error {
	start := s.pos
	depth := 0
	for s.pos < len(s.src) {
		switch {
		case s.src[s.pos] == '/' && s.peekAt(1) == '*':
			depth++
			s.pos += 2
		case s.src[s.pos] == '*' && s.peekAt(1) == '/':
			depth--
			s.pos += 2
			if depth == 0 {
				return nil
			}
		default:
			s.pos++
		}
	}
	return pgerr.Syntaxf(token.Pos(start), "unterminated /* comment")
}

func (s *Scanner) scanIdent() token.Token {
	start := s.pos
	for s.pos < len(s.src) && isIdentPart(s.src[s.pos]) {
		s.pos++
	}
	raw := s.src[start:s.pos]
	name := foldASCII(raw)
	return token.Token{Kind: token.Lookup(name), Pos: token.Pos(start), Val: name}
}

// scanNumber scans an integer, decimal or exponent literal. The sign is never
// consumed here; see the package comment.
func (s *Scanner) scanNumber() (token.Token, error) {
	start := s.pos
	for s.pos < len(s.src) && isDigit(s.src[s.pos]) {
		s.pos++
	}
	if s.pos < len(s.src) && s.src[s.pos] == '.' {
		s.pos++
		for s.pos < len(s.src) && isDigit(s.src[s.pos]) {
			s.pos++
		}
	}
	// An exponent only counts if it is well-formed. Otherwise back off and let
	// "1e" scan as the number 1 followed by the identifier e, which yields a
	// better parse error than failing in the scanner.
	if s.pos < len(s.src) && (s.src[s.pos] == 'e' || s.src[s.pos] == 'E') {
		mark := s.pos
		s.pos++
		if s.pos < len(s.src) && (s.src[s.pos] == '+' || s.src[s.pos] == '-') {
			s.pos++
		}
		if s.pos < len(s.src) && isDigit(s.src[s.pos]) {
			for s.pos < len(s.src) && isDigit(s.src[s.pos]) {
				s.pos++
			}
		} else {
			s.pos = mark
		}
	}
	// "1abc" is a lexing error rather than 1 followed by abc, matching
	// PostgreSQL: an identifier may not start immediately after a numeral.
	if s.pos < len(s.src) && isIdentStart(s.src[s.pos]) {
		return token.Token{}, pgerr.Syntaxf(token.Pos(s.pos),
			"trailing junk after numeric literal")
	}
	return token.Token{Kind: token.Number, Pos: token.Pos(start), Val: s.src[start:s.pos]}, nil
}

// scanString scans a quoted literal. A doubled single quote is an escaped
// quote. When escapes is true the literal was introduced by E and backslash
// escapes apply as well.
func (s *Scanner) scanString(escapes bool) (token.Token, error) {
	start := s.pos
	s.pos++ // opening quote
	var b strings.Builder
	for {
		if s.pos >= len(s.src) {
			return token.Token{}, pgerr.Syntaxf(token.Pos(start), "unterminated quoted string")
		}
		c := s.src[s.pos]
		switch {
		case c == '\'':
			if s.peekAt(1) == '\'' {
				b.WriteByte('\'')
				s.pos += 2
				continue
			}
			s.pos++
			return token.Token{Kind: token.String, Pos: token.Pos(start), Val: b.String()}, nil
		case escapes && c == '\\':
			s.pos++
			if s.pos >= len(s.src) {
				return token.Token{}, pgerr.Syntaxf(token.Pos(start), "unterminated quoted string")
			}
			b.WriteByte(unescape(s.src[s.pos]))
			s.pos++
		default:
			b.WriteByte(c)
			s.pos++
		}
	}
}

func unescape(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'r':
		return '\r'
	case 'b':
		return '\b'
	case 'f':
		return '\f'
	default:
		// Including \\ and \', which stand for themselves.
		return c
	}
}

// scanQuotedIdent scans a "quoted identifier", which preserves case and treats
// "" as an escaped quote.
func (s *Scanner) scanQuotedIdent() (token.Token, error) {
	start := s.pos
	s.pos++ // opening quote
	var b strings.Builder
	for {
		if s.pos >= len(s.src) {
			return token.Token{}, pgerr.Syntaxf(token.Pos(start), "unterminated quoted identifier")
		}
		c := s.src[s.pos]
		if c == '"' {
			if s.peekAt(1) == '"' {
				b.WriteByte('"')
				s.pos += 2
				continue
			}
			s.pos++
			if b.Len() == 0 {
				return token.Token{}, pgerr.Syntaxf(token.Pos(start), "zero-length delimited identifier")
			}
			return token.Token{Kind: token.QuotedIdent, Pos: token.Pos(start), Val: b.String()}, nil
		}
		b.WriteByte(c)
		s.pos++
	}
}

// scanDollar disambiguates the three things a $ can introduce: a $1 parameter,
// a $$ dollar-quoted string, or a $tag$ dollar-quoted string.
func (s *Scanner) scanDollar() (token.Token, error) {
	start := s.pos
	if isDigit(s.peekAt(1)) {
		s.pos++ // $
		numStart := s.pos
		for s.pos < len(s.src) && isDigit(s.src[s.pos]) {
			s.pos++
		}
		ord, ok := atoi(s.src[numStart:s.pos])
		if !ok || ord == 0 {
			return token.Token{}, pgerr.Syntaxf(token.Pos(start), "invalid parameter number")
		}
		if s.usedQM {
			return token.Token{}, pgerr.Syntaxf(token.Pos(start),
				"cannot mix $N and ? parameter placeholders in one statement")
		}
		s.usedDollar = true
		return token.Token{Kind: token.Param, Pos: token.Pos(start), Ord: ord}, nil
	}
	return s.scanDollarQuoted()
}

// scanDollarQuoted scans $tag$...$tag$, where tag may be empty.
func (s *Scanner) scanDollarQuoted() (token.Token, error) {
	start := s.pos
	i := s.pos + 1
	for i < len(s.src) && s.src[i] != '$' {
		if !isIdentPart(s.src[i]) {
			return token.Token{}, pgerr.Syntaxf(token.Pos(start), "invalid dollar-quote tag")
		}
		i++
	}
	if i >= len(s.src) {
		return token.Token{}, pgerr.Syntaxf(token.Pos(start), "unterminated dollar-quoted string")
	}
	delim := s.src[start : i+1] // "$" tag "$"
	body := i + 1
	end := strings.Index(s.src[body:], delim)
	if end < 0 {
		return token.Token{}, pgerr.Syntaxf(token.Pos(start), "unterminated dollar-quoted string")
	}
	// The body is taken verbatim: dollar quoting exists precisely so that no
	// escape processing happens. Notably the contents are never sniffed to guess
	// a type — $$123$$ is the string "123".
	val := s.src[body : body+end]
	s.pos = body + end + len(delim)
	return token.Token{Kind: token.String, Pos: token.Pos(start), Val: val}, nil
}

// questionParam assigns the next ordinal to a ? placeholder.
func (s *Scanner) questionParam(pos token.Pos) (token.Token, error) {
	if s.usedDollar {
		return token.Token{}, pgerr.Syntaxf(pos,
			"cannot mix $N and ? parameter placeholders in one statement")
	}
	s.usedQM = true
	ord := s.nextOrd
	s.nextOrd++
	return token.Token{Kind: token.Param, Pos: pos, Ord: ord}, nil
}

// operators is ordered longest-first within each starting byte so that a
// two-character operator is never mis-scanned as one character followed by
// another. Unlike a global matcher list, the order here is local and obvious.
var operators = []struct {
	text string
	kind token.Kind
}{
	{"::", token.DoubleColon},
	{"||", token.Concat},
	{"<>", token.NotEq},
	{"!=", token.NotEq},
	{"<=", token.LessEq},
	{">=", token.GreaterEq},
	{"(", token.LParen},
	{")", token.RParen},
	{",", token.Comma},
	{";", token.Semicolon},
	{".", token.Dot},
	{":", token.Colon},
	{"+", token.Plus},
	{"-", token.Minus},
	{"*", token.Star},
	{"/", token.Slash},
	{"%", token.Percent},
	{"^", token.Caret},
	{"=", token.Eq},
	{"<", token.Less},
	{">", token.Greater},
}

func (s *Scanner) scanOperator() (token.Token, error) {
	rest := s.src[s.pos:]
	for _, op := range operators {
		if strings.HasPrefix(rest, op.text) {
			pos := s.pos
			s.pos += len(op.text)
			return token.Token{Kind: op.kind, Pos: token.Pos(pos)}, nil
		}
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return token.Token{}, pgerr.Syntaxf(token.Pos(s.pos), "unexpected character %q", r)
}

// peekAt returns the byte n positions ahead, or 0 at end of input. Returning 0
// is safe because a NUL byte is not valid anywhere in SQL text we accept.
func (s *Scanner) peekAt(n int) byte {
	if s.pos+n >= len(s.src) {
		return 0
	}
	return s.src[s.pos+n]
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// isIdentStart reports whether c may begin an identifier. Bytes >= 0x80 are
// accepted so that multi-byte letters work; validity of the decoded rune is
// checked by foldASCII's slow path.
func isIdentStart(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || c >= utf8.RuneSelf
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

// foldASCII lower-cases an identifier the way PostgreSQL folds unquoted names.
// The common case is pure ASCII, which avoids allocating when already folded.
func foldASCII(s string) string {
	needs := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			needs = true
		} else if c >= utf8.RuneSelf {
			return strings.ToLower(s)
		}
	}
	if !needs {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// atoi parses a run of digits, reporting failure instead of overflowing.
func atoi(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}
