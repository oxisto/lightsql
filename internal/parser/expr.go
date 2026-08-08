package parser

import (
	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/token"
)

// Binding powers, lowest first. These reproduce PostgreSQL's operator
// precedence table exactly, including the two details that are easy to get
// wrong: exponentiation is left associative in PostgreSQL (unlike ordinary
// mathematical notation), and unary minus binds tighter than exponentiation, so
// -2^2 is 4 rather than -4.
//
// Encoding precedence as a table consumed by one loop is what makes the grammar
// checkable. Handling operators ad hoc at each call site, as a hand-rolled
// "check for an operator, then check for another" parser does, silently drops
// operands in expressions such as a * b + c and forces the same code to be
// repeated for WHERE, HAVING and the select list.
const (
	bpNone = iota
	bpOr
	bpAnd
	bpNot
	bpIs
	bpCompare
	bpRange // BETWEEN, IN, LIKE
	bpAdd
	bpMul
	bpExp
	bpUnary
	bpCast
)

// infixOps maps a token to the binary operator it introduces and its binding
// power. Operators absent from this table terminate an expression.
var infixOps = map[token.Kind]struct {
	op ast.BinaryOp
	bp int
}{
	token.Or:        {ast.OpOr, bpOr},
	token.And:       {ast.OpAnd, bpAnd},
	token.Eq:        {ast.OpEq, bpCompare},
	token.NotEq:     {ast.OpNe, bpCompare},
	token.Less:      {ast.OpLt, bpCompare},
	token.LessEq:    {ast.OpLe, bpCompare},
	token.Greater:   {ast.OpGt, bpCompare},
	token.GreaterEq: {ast.OpGe, bpCompare},
	token.Plus:      {ast.OpAdd, bpAdd},
	token.Minus:     {ast.OpSub, bpAdd},
	token.Star:      {ast.OpMul, bpMul},
	token.Slash:     {ast.OpDiv, bpMul},
	token.Percent:   {ast.OpMod, bpMul},
	token.Caret:     {ast.OpExp, bpExp},
	token.Concat:    {ast.OpConcat, bpAdd},
}

// parseExpr parses an expression, consuming operators that bind at least as
// tightly as minBP. Callers start at bpNone.
func (p *parser) parseExpr(minBP int) (ast.Expr, error) {
	lhs, err := p.parsePrefix()
	if err != nil {
		return nil, err
	}
	return p.parseInfix(lhs, minBP)
}

// parseInfix repeatedly extends lhs with operators at or above minBP.
func (p *parser) parseInfix(lhs ast.Expr, minBP int) (ast.Expr, error) {
	for {
		tok := p.cur()

		// Postfix and mixfix forms are handled before the plain binary table,
		// because their leading token also appears elsewhere.
		switch tok.Kind {
		case token.DoubleColon:
			if bpCast < minBP {
				return lhs, nil
			}
			p.next()
			typ, err := p.parseTypeName()
			if err != nil {
				return nil, err
			}
			lhs = &ast.CastExpr{CastPos: tok.Pos, X: lhs, Type: typ}
			continue

		case token.Is:
			if bpIs < minBP {
				return lhs, nil
			}
			var err error
			if lhs, err = p.parseIs(lhs); err != nil {
				return nil, err
			}
			continue

		case token.In, token.Between, token.Like:
			if bpRange < minBP {
				return lhs, nil
			}
			var err error
			if lhs, err = p.parseRange(lhs, false); err != nil {
				return nil, err
			}
			continue

		case token.Not:
			// In infix position NOT is not the prefix operator; it must be the
			// negating half of NOT IN, NOT LIKE or NOT BETWEEN.
			switch p.peek(1).Kind {
			case token.In, token.Between, token.Like:
				if bpRange < minBP {
					return lhs, nil
				}
				p.next()
				var err error
				if lhs, err = p.parseRange(lhs, true); err != nil {
					return nil, err
				}
				continue
			}
			return lhs, nil
		}

		info, ok := infixOps[tok.Kind]
		if !ok || info.bp < minBP {
			return lhs, nil
		}
		p.next()
		// Every operator here is left associative, so the right operand is
		// parsed at one level tighter, which stops it from swallowing the next
		// operator of equal precedence.
		rhs, err := p.parseExpr(info.bp + 1)
		if err != nil {
			return nil, err
		}
		lhs = &ast.BinaryExpr{Op: info.op, OpPos: tok.Pos, X: lhs, Y: rhs}
	}
}

// parseIs handles the IS family, which shares a leading token but produces
// different nodes: IS NULL is not a comparison against NULL, since it yields
// TRUE or FALSE rather than UNKNOWN.
func (p *parser) parseIs(lhs ast.Expr) (ast.Expr, error) {
	isPos := p.cur().Pos
	p.next()
	negate := p.accept(token.Not)

	switch {
	case p.accept(token.Null):
		return &ast.IsNullExpr{X: lhs, IsPos: isPos, Negate: negate}, nil

	case p.at(token.True), p.at(token.False):
		// x IS TRUE is exactly x IS NOT DISTINCT FROM TRUE, so desugar rather
		// than carrying a node whose semantics would have to be re-derived.
		lit := p.boolLiteral()
		op := ast.OpIsNotDistinctFrom
		if negate {
			op = ast.OpIsDistinctFrom
		}
		return &ast.BinaryExpr{Op: op, OpPos: isPos, X: lhs, Y: lit}, nil

	case p.accept(token.Distinct):
		if _, err := p.expect(token.From); err != nil {
			return nil, err
		}
		rhs, err := p.parseExpr(bpIs + 1)
		if err != nil {
			return nil, err
		}
		op := ast.OpIsDistinctFrom
		if negate {
			op = ast.OpIsNotDistinctFrom
		}
		return &ast.BinaryExpr{Op: op, OpPos: isPos, X: lhs, Y: rhs}, nil
	}

	return nil, p.unexpected("NULL, TRUE, FALSE or DISTINCT FROM")
}

// parseRange handles IN, BETWEEN and LIKE, whose negated forms are written with
// a NOT that the caller has already consumed.
func (p *parser) parseRange(lhs ast.Expr, negate bool) (ast.Expr, error) {
	tok := p.cur()
	p.next()

	switch tok.Kind {
	case token.Like:
		rhs, err := p.parseExpr(bpRange + 1)
		if err != nil {
			return nil, err
		}
		op := ast.OpLike
		if negate {
			op = ast.OpNotLike
		}
		return &ast.BinaryExpr{Op: op, OpPos: tok.Pos, X: lhs, Y: rhs}, nil

	case token.Between:
		// BETWEEN binds its operands tighter than AND, so that the AND joining
		// the bounds is not mistaken for a logical conjunction.
		lo, err := p.parseExpr(bpRange + 1)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(token.And); err != nil {
			return nil, err
		}
		hi, err := p.parseExpr(bpRange + 1)
		if err != nil {
			return nil, err
		}
		return &ast.BetweenExpr{X: lhs, BetweenPos: tok.Pos, Negate: negate, Lo: lo, Hi: hi}, nil

	default: // token.In
		if _, err := p.expect(token.LParen); err != nil {
			return nil, err
		}
		in := &ast.InExpr{X: lhs, InPos: tok.Pos, Negate: negate}
		if p.at(token.Select) {
			sel, err := p.parseSelect()
			if err != nil {
				return nil, err
			}
			in.Subquery = sel
		} else {
			for {
				e, err := p.parseExpr(bpNone)
				if err != nil {
					return nil, err
				}
				in.List = append(in.List, e)
				if !p.accept(token.Comma) {
					break
				}
			}
		}
		if _, err := p.expect(token.RParen); err != nil {
			return nil, err
		}
		return in, nil
	}
}

// parsePrefix parses a primary expression together with any prefix operators.
func (p *parser) parsePrefix() (ast.Expr, error) {
	tok := p.cur()

	switch tok.Kind {
	case token.Not:
		p.next()
		x, err := p.parseExpr(bpNot)
		if err != nil {
			return nil, err
		}
		return &ast.UnaryExpr{Op: ast.OpNot, OpPos: tok.Pos, X: x}, nil

	case token.Minus, token.Plus:
		p.next()
		// Unary sign binds tighter than exponentiation in PostgreSQL.
		x, err := p.parseExpr(bpUnary)
		if err != nil {
			return nil, err
		}
		op := ast.OpNeg
		if tok.Kind == token.Plus {
			op = ast.OpPlus
		}
		return &ast.UnaryExpr{Op: op, OpPos: tok.Pos, X: x}, nil

	case token.String:
		p.next()
		return &ast.Literal{ValuePos: tok.Pos, Kind: ast.LitString, Val: tok.Val}, nil

	case token.Number:
		p.next()
		return &ast.Literal{ValuePos: tok.Pos, Kind: ast.LitNumber, Val: tok.Val}, nil

	case token.True, token.False:
		return p.boolLiteral(), nil

	case token.Null:
		p.next()
		return &ast.Literal{ValuePos: tok.Pos, Kind: ast.LitNull}, nil

	case token.Param:
		p.next()
		return &ast.Param{ParamPos: tok.Pos, Ord: tok.Ord}, nil

	case token.Star:
		p.next()
		return &ast.Star{StarPos: tok.Pos}, nil

	case token.LParen:
		return p.parseParen()

	case token.Case:
		return p.parseCase()

	case token.Exists:
		p.next()
		sub, err := p.parseParenSelect()
		if err != nil {
			return nil, err
		}
		return &ast.ExistsExpr{ExistsPos: tok.Pos, Subquery: sub}, nil

	case token.Cast:
		return p.parseCastCall()

	case token.Ident, token.QuotedIdent:
		return p.parseIdentExpr()
	}

	return nil, p.unexpected("an expression")
}

func (p *parser) boolLiteral() *ast.Literal {
	tok := p.cur()
	p.next()
	kind := ast.LitTrue
	if tok.Kind == token.False {
		kind = ast.LitFalse
	}
	return &ast.Literal{ValuePos: tok.Pos, Kind: kind}
}

// parseParen handles both a parenthesised expression and a parenthesised
// subquery, which are only distinguishable by looking past the paren.
func (p *parser) parseParen() (ast.Expr, error) {
	lparen := p.cur().Pos
	p.next()

	if p.at(token.Select) {
		sel, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(token.RParen); err != nil {
			return nil, err
		}
		return &ast.SubqueryExpr{Lparen: lparen, Select: sel}, nil
	}

	x, err := p.parseExpr(bpNone)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(token.RParen); err != nil {
		return nil, err
	}
	return &ast.ParenExpr{Lparen: lparen, X: x}, nil
}

func (p *parser) parseParenSelect() (*ast.SelectStmt, error) {
	if _, err := p.expect(token.LParen); err != nil {
		return nil, err
	}
	sel, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(token.RParen); err != nil {
		return nil, err
	}
	return sel, nil
}

// parseCase parses both the simple form, CASE x WHEN v THEN ..., and the
// searched form, CASE WHEN cond THEN ....
func (p *parser) parseCase() (ast.Expr, error) {
	casePos := p.cur().Pos
	p.next()

	c := &ast.CaseExpr{CasePos: casePos}
	if p.at(token.End) {
		// Reported here rather than letting the operand parse fail, so the
		// message names the clause that is actually missing.
		return nil, p.unexpected("WHEN")
	}
	if !p.at(token.When) {
		operand, err := p.parseExpr(bpNone)
		if err != nil {
			return nil, err
		}
		c.Operand = operand
	}

	for p.at(token.When) {
		whenPos := p.cur().Pos
		p.next()
		cond, err := p.parseExpr(bpNone)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(token.Then); err != nil {
			return nil, err
		}
		val, err := p.parseExpr(bpNone)
		if err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, &ast.WhenClause{WhenPos: whenPos, Cond: cond, Value: val})
	}
	if len(c.Whens) == 0 {
		return nil, p.unexpected("WHEN")
	}

	if p.accept(token.Else) {
		els, err := p.parseExpr(bpNone)
		if err != nil {
			return nil, err
		}
		c.Else = els
	}
	if _, err := p.expect(token.End); err != nil {
		return nil, err
	}
	return c, nil
}

// parseCastCall parses the CAST(x AS type) spelling; the :: spelling is handled
// as a postfix operator.
func (p *parser) parseCastCall() (ast.Expr, error) {
	castPos := p.cur().Pos
	p.next()
	if _, err := p.expect(token.LParen); err != nil {
		return nil, err
	}
	x, err := p.parseExpr(bpNone)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(token.As); err != nil {
		return nil, err
	}
	typ, err := p.parseTypeName()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(token.RParen); err != nil {
		return nil, err
	}
	return &ast.CastExpr{CastPos: castPos, X: x, Type: typ}, nil
}

// parseIdentExpr parses a name, which may be a column reference of up to three
// parts or a function call.
func (p *parser) parseIdentExpr() (ast.Expr, error) {
	first := p.name()

	if p.at(token.LParen) {
		return p.parseCall(first)
	}

	ref := &ast.ColumnRef{Column: first}
	if !p.accept(token.Dot) {
		return ref, nil
	}

	// t.* is a qualified star rather than a column.
	if p.at(token.Star) {
		starPos := p.cur().Pos
		p.next()
		return &ast.Star{StarPos: starPos, Table: first}, nil
	}

	second, err := p.expectName()
	if err != nil {
		return nil, err
	}
	ref = &ast.ColumnRef{Table: first, Column: second}
	if !p.accept(token.Dot) {
		return ref, nil
	}

	if p.at(token.Star) {
		starPos := p.cur().Pos
		p.next()
		return &ast.Star{StarPos: starPos, Table: second}, nil
	}
	third, err := p.expectName()
	if err != nil {
		return nil, err
	}
	return &ast.ColumnRef{Schema: first, Table: second, Column: third}, nil
}

func (p *parser) parseCall(name ast.Name) (ast.Expr, error) {
	lparen := p.cur().Pos
	p.next()

	call := &ast.FuncCall{Name: name, Lparen: lparen}
	switch {
	case p.at(token.Star):
		// COUNT(*) takes no argument expression.
		p.next()
		call.Star = true
	case p.at(token.RParen):
		// Zero-argument call such as now().
	default:
		call.Distinct = p.accept(token.Distinct)
		for {
			arg, err := p.parseExpr(bpNone)
			if err != nil {
				return nil, err
			}
			call.Args = append(call.Args, arg)
			if !p.accept(token.Comma) {
				break
			}
		}
	}
	if _, err := p.expect(token.RParen); err != nil {
		return nil, err
	}
	return call, nil
}

// multiWordTypes lists the type names PostgreSQL spells with more than one
// word. Recognising them here keeps the scanner free of type knowledge.
var multiWordTypes = map[string][]string{
	"double":    {"precision"},
	"character": {"varying"},
	"bit":       {"varying"},
}

// parseTypeName parses a written type, including multi-word spellings, optional
// precision modifiers and an optional time zone suffix.
func (p *parser) parseTypeName() (*ast.TypeName, error) {
	tok := p.cur()
	if tok.Kind != token.Ident && tok.Kind != token.QuotedIdent {
		return nil, p.unexpected("a type name")
	}
	p.next()
	name := tok.Val

	if follow, ok := multiWordTypes[name]; ok {
		if p.at(token.Ident) && p.cur().Val == follow[0] {
			name += " " + follow[0]
			p.next()
		} else if name == "character" {
			// Bare "character" is CHAR; "character varying" is VARCHAR.
			name = "char"
		}
	}

	typ := &ast.TypeName{NamePos: tok.Pos, Name: name}

	if p.accept(token.LParen) {
		for {
			n := p.cur()
			if n.Kind != token.Number {
				return nil, p.unexpected("a type modifier")
			}
			p.next()
			mod, ok := atoiStrict(n.Val)
			if !ok {
				return nil, pgerr.Syntaxf(n.Pos, "invalid type modifier %q", n.Val)
			}
			typ.Mods = append(typ.Mods, mod)
			if !p.accept(token.Comma) {
				break
			}
		}
		if _, err := p.expect(token.RParen); err != nil {
			return nil, err
		}
	}

	// "timestamp with time zone" and "time without time zone" are suffixes
	// rather than separate type names.
	if name == "timestamp" || name == "time" {
		if p.at(token.Ident) && (p.cur().Val == "with" || p.cur().Val == "without") {
			with := p.cur().Val == "with"
			p.next()
			if err := p.expectWord("time"); err != nil {
				return nil, err
			}
			if err := p.expectWord("zone"); err != nil {
				return nil, err
			}
			if with {
				name += "tz"
			}
			typ.Name = name
		}
	}

	return typ, nil
}

func atoiStrict(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	n := 0
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
