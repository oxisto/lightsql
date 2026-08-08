// Package token defines the lexical tokens of the PostgreSQL dialect understood
// by lightsql, along with the source positions attached to them.
package token

import (
	"strconv"
	"strings"
)

// Kind classifies a token. It is a distinct type rather than a bare int so that
// a token kind can never be silently confused with an ordinal, a column index or
// any other integer flying around the parser.
type Kind uint16

const (
	// Invalid is the zero value and never appears in a well-formed token stream.
	Invalid Kind = iota
	// EOF terminates every token stream, so the parser can always look ahead
	// one token without a bounds check.
	EOF

	literalBegin
	// Ident is an unquoted identifier. Its Val is already lower-cased, matching
	// PostgreSQL's folding rules.
	Ident
	// QuotedIdent is a "double quoted" identifier. Its Val preserves case and has
	// "" escapes resolved.
	QuotedIdent
	// String is a 'string literal' or a $tag$dollar quoted$tag$ one, with ''
	// escapes resolved.
	String
	// Number is a numeric literal. The sign is never folded in: -1 scans as
	// Minus followed by Number, so that a-1 and a - 1 agree.
	Number
	// Param is a $1 or ? placeholder. Ordinals are assigned here, at scan time,
	// so nothing downstream needs a mutable counter.
	Param
	literalEnd

	operatorBegin
	// Punctuation.
	LParen      // (
	RParen      // )
	Comma       // ,
	Semicolon   // ;
	Dot         // .
	Colon       // :
	DoubleColon // :: — the cast operator, distinct from the CAST keyword

	// Arithmetic.
	Plus    // +
	Minus   // -
	Star    // *
	Slash   // /
	Percent // %
	Caret   // ^

	// Comparison.
	Eq    // =
	NotEq // <> or !=
	Less  // <
	LessEq
	Greater
	GreaterEq

	// String and misc operators.
	Concat // ||
	operatorEnd

	keywordBegin
	All
	And
	As
	Asc
	Between
	By
	Case
	Cast
	Check
	Constraint
	Create
	Cross
	Default
	Delete
	Desc
	Distinct
	Drop
	Else
	End
	Exists
	False
	Foreign
	From
	Full
	Group
	Having
	If
	In
	Inner
	Insert
	Intersect
	Into
	Is
	Join
	Key
	Left
	Like
	Limit
	Not
	Null
	Offset
	On
	Or
	Order
	Outer
	Primary
	References
	Returning
	Right
	Select
	Set
	Table
	Then
	True
	Union
	Unique
	Update
	Using
	Values
	When
	Where
	keywordEnd
)

// kindNames is built by a function rather than an init, so the dependency on
// keywords is expressed directly and package initialisation order handles it.
var kindNames = buildKindNames()

func buildKindNames() map[Kind]string {
	names := map[Kind]string{
		Invalid:     "INVALID",
		EOF:         "EOF",
		Ident:       "IDENT",
		QuotedIdent: "QUOTED_IDENT",
		String:      "STRING",
		Number:      "NUMBER",
		Param:       "PARAM",

		LParen:      "(",
		RParen:      ")",
		Comma:       ",",
		Semicolon:   ";",
		Dot:         ".",
		Colon:       ":",
		DoubleColon: "::",
		Plus:        "+",
		Minus:       "-",
		Star:        "*",
		Slash:       "/",
		Percent:     "%",
		Caret:       "^",
		Eq:          "=",
		NotEq:       "<>",
		Less:        "<",
		LessEq:      "<=",
		Greater:     ">",
		GreaterEq:   ">=",
		Concat:      "||",
	}
	// A keyword's name is just its upper-cased source text, so derive it rather
	// than maintaining a second table that can drift out of sync.
	for text, kind := range keywords {
		names[kind] = strings.ToUpper(text)
	}
	return names
}

// keywords maps the lower-cased keyword text to its kind. A single map lookup
// after scanning an identifier replaces the per-keyword matcher functions that
// make keyword/identifier collisions (COUNT vs COUNTRY) an ordering hazard.
var keywords = map[string]Kind{
	"all": All, "and": And, "as": As, "asc": Asc, "between": Between,
	"by": By, "case": Case, "cast": Cast, "check": Check,
	"constraint": Constraint, "create": Create, "cross": Cross,
	"default": Default,
	"delete":  Delete, "desc": Desc, "distinct": Distinct, "drop": Drop,
	"else": Else, "end": End, "exists": Exists, "false": False,
	"foreign": Foreign, "from": From,
	"full": Full, "group": Group, "having": Having, "if": If, "in": In,
	"inner": Inner, "insert": Insert, "intersect": Intersect, "into": Into,
	"is": Is, "join": Join, "key": Key, "left": Left, "like": Like,
	"limit": Limit, "not": Not, "null": Null, "offset": Offset, "on": On,
	"or": Or, "order": Order, "outer": Outer, "primary": Primary,
	"references": References, "returning": Returning, "right": Right,
	"select": Select, "set": Set, "table": Table, "then": Then, "true": True,
	"union": Union, "unique": Unique, "update": Update, "using": Using,
	"values": Values,
	"when":   When, "where": Where,
}

// Lookup returns the keyword kind for an identifier, or Ident if it is not a
// keyword. name must already be lower-cased.
func Lookup(name string) Kind {
	if k, ok := keywords[name]; ok {
		return k
	}
	return Ident
}

// String returns the canonical text of the kind, suitable for error messages.
func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// IsLiteral reports whether the kind is a literal or identifier token, i.e. one
// whose Val carries meaning beyond the kind itself.
func (k Kind) IsLiteral() bool { return literalBegin < k && k < literalEnd }

// IsOperator reports whether the kind is an operator or punctuation token.
func (k Kind) IsOperator() bool { return operatorBegin < k && k < operatorEnd }

// IsKeyword reports whether the kind is a reserved or unreserved keyword.
func (k Kind) IsKeyword() bool { return keywordBegin < k && k < keywordEnd }

// Pos is a byte offset into the SQL text being scanned, zero-based. Every token
// and every AST node carries one so that errors can point at the source, which
// in turn means the original text must never be rewritten or stripped before
// parsing.
type Pos int

// NoPos is the zero Pos, used for synthesised nodes that have no source text.
const NoPos Pos = -1

// IsValid reports whether p refers to a real position in the source.
func (p Pos) IsValid() bool { return p >= 0 }

// Token is a single lexical token.
type Token struct {
	Kind Kind
	Pos  Pos
	// Val holds the semantic value: the folded identifier name, the resolved
	// string contents (escapes already processed), or the numeric text. It is
	// empty for operators and keywords, whose Kind says everything.
	Val string
	// Ord is the 1-based parameter ordinal, set only when Kind is Param.
	Ord int
}
