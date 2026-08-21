// Package parser turns a token stream into a typed AST.
//
// The parser is recursive descent for statements and clauses, with a Pratt
// (precedence climbing) core for expressions — see expr.go. The two halves are
// separate on purpose: statement structure is a fixed sequence of clauses and
// reads best as straight-line code, while expression structure is entirely
// determined by an operator precedence table and reads worst that way.
//
// The parser never mutates the token stream and never rewrites a node it has
// already produced. Everything it needs to decide a production is available from
// bounded lookahead.
package parser

import (
	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/scanner"
	"github.com/oxisto/lightsql/internal/token"
)

// Parse parses one or more semicolon-separated statements.
func Parse(src string) ([]ast.Stmt, error) {
	toks, err := scanner.Tokens(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks}

	var stmts []ast.Stmt
	for {
		for p.accept(token.Semicolon) {
		}
		if p.at(token.EOF) {
			break
		}
		stmt, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, stmt)
		if !p.at(token.EOF) && !p.at(token.Semicolon) {
			return nil, p.unexpected("end of statement")
		}
	}
	if len(stmts) == 0 {
		return nil, pgerr.Syntaxf(token.Pos(len(src)), "empty query")
	}
	return stmts, nil
}

// ParseOne parses exactly one statement, rejecting a batch. The driver uses it
// on the prepared-statement path, where multiple statements cannot be executed.
func ParseOne(src string) (ast.Stmt, error) {
	stmts, err := Parse(src)
	if err != nil {
		return nil, err
	}
	if len(stmts) > 1 {
		return nil, pgerr.Syntaxf(stmts[1].Pos(), "cannot insert multiple commands into a prepared statement")
	}
	return stmts[0], nil
}

// ScanParams reports the highest parameter ordinal a statement uses, which is
// the number of arguments it expects.
//
// It works from the token stream rather than counting occurrences of "$" or "?"
// in the text, which would also match inside string literals. Taking the maximum
// rather than the count is what makes a statement reusing $1 twice ask for one
// argument rather than two.
func ScanParams(sql string) (int, error) {
	toks, err := scanner.Tokens(sql)
	if err != nil {
		return 0, err
	}
	highest := 0
	for _, t := range toks {
		if t.Kind == token.Param && t.Ord > highest {
			highest = t.Ord
		}
	}
	return highest, nil
}

type parser struct {
	src  string
	toks []token.Token
	i    int
}

// cur returns the current token. The stream always ends with EOF, so this is
// safe without a bounds check.
func (p *parser) cur() token.Token { return p.toks[p.i] }

// peek returns the token n positions ahead, clamped to the terminating EOF.
func (p *parser) peek(n int) token.Token {
	if p.i+n >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.i+n]
}

func (p *parser) next() {
	if p.i < len(p.toks)-1 {
		p.i++
	}
}

func (p *parser) at(k token.Kind) bool { return p.toks[p.i].Kind == k }

// accept consumes the current token if it has the given kind.
func (p *parser) accept(k token.Kind) bool {
	if p.at(k) {
		p.next()
		return true
	}
	return false
}

// expect consumes the current token, which must have the given kind.
//
// It returns only an error: the token itself is fully described by the kind the
// caller just demanded, so returning it invited call sites to write a blank
// identifier for a value that could never be interesting.
func (p *parser) expect(k token.Kind) error {
	if !p.at(k) {
		return p.unexpected(k.String())
	}
	p.next()
	return nil
}

// expectWord consumes an unreserved word, i.e. an identifier used as syntax.
// Words such as "zone" and "nulls" are not reserved keywords, so they arrive as
// identifiers and are matched by value.
func (p *parser) expectWord(word string) error {
	if !p.at(token.Ident) || p.cur().Val != word {
		return p.unexpected(word)
	}
	p.next()
	return nil
}

// atWord reports whether the current token is the given unreserved word.
func (p *parser) atWord(word string) bool {
	return p.at(token.Ident) && p.cur().Val == word
}

// name consumes the current identifier token and returns it as a Name. The
// caller must already have checked the kind.
func (p *parser) name() ast.Name {
	tok := p.cur()
	p.next()
	return ast.Name{NamePos: tok.Pos, Name: tok.Val, Quoted: tok.Kind == token.QuotedIdent}
}

// expectName consumes an identifier, quoted or not.
func (p *parser) expectName() (ast.Name, error) {
	if !p.at(token.Ident) && !p.at(token.QuotedIdent) {
		return ast.Name{}, p.unexpected("an identifier")
	}
	return p.name(), nil
}

// unexpected reports a syntax error at the current token, naming what was
// expected. The message mirrors PostgreSQL's "syntax error at or near" form so
// that parity tests can compare them, and it always carries a position — which
// is only possible because the scanner never discarded one.
func (p *parser) unexpected(want string) error {
	tok := p.cur()
	if tok.Kind == token.EOF {
		return pgerr.Syntaxf(tok.Pos, "syntax error at end of input, expected %s", want)
	}
	return pgerr.Syntaxf(tok.Pos, "syntax error at or near %q, expected %s", p.text(tok), want)
}

// text renders a token as it appeared in the source, for error messages.
func (p *parser) text(tok token.Token) string {
	switch tok.Kind {
	case token.Ident, token.QuotedIdent, token.Number, token.String:
		return tok.Val
	default:
		return tok.Kind.String()
	}
}

func (p *parser) parseStmt() (ast.Stmt, error) {
	switch p.cur().Kind {
	case token.Select:
		return p.parseSelect()
	case token.Insert:
		return p.parseInsert()
	case token.Update:
		return p.parseUpdate()
	case token.Delete:
		return p.parseDelete()
	case token.Create:
		return p.parseCreate()
	case token.Drop:
		return p.parseDrop()
	default:
		return nil, p.unexpected("SELECT, INSERT, UPDATE, DELETE, CREATE or DROP")
	}
}

// parseDrop parses DROP TABLE.
//
// Several tables may be named at once, and PostgreSQL drops them as one
// statement rather than in sequence, so a reference between two of them is not
// a reason to refuse.
func (p *parser) parseDrop() (ast.Stmt, error) {
	stmt := &ast.DropTableStmt{DropPos: p.cur().Pos}
	p.next()
	if err := p.expect(token.Table); err != nil {
		return nil, err
	}

	if p.accept(token.If) {
		if err := p.expect(token.Exists); err != nil {
			return nil, err
		}
		stmt.IfExists = true
	}

	for {
		name, err := p.parseTableName()
		if err != nil {
			return nil, err
		}
		stmt.Tables = append(stmt.Tables, name)
		if !p.accept(token.Comma) {
			break
		}
	}

	// RESTRICT is the default and says so explicitly; both are accepted so a
	// statement written either way parses.
	switch {
	case p.atWord("cascade"):
		p.next()
		stmt.Cascade = true
	case p.atWord("restrict"):
		p.next()
	}
	return stmt, nil
}

// ---------------------------------------------------------------------------
// SELECT
// ---------------------------------------------------------------------------

func (p *parser) parseSelect() (*ast.SelectStmt, error) {
	sel := &ast.SelectStmt{SelectPos: p.cur().Pos}
	if err := p.expect(token.Select); err != nil {
		return nil, err
	}

	if p.accept(token.Distinct) {
		sel.Distinct = true
		if p.accept(token.On) {
			if err := p.expect(token.LParen); err != nil {
				return nil, err
			}
			for {
				e, err := p.parseExpr(bpNone)
				if err != nil {
					return nil, err
				}
				sel.DistinctOn = append(sel.DistinctOn, e)
				if !p.accept(token.Comma) {
					break
				}
			}
			if err := p.expect(token.RParen); err != nil {
				return nil, err
			}
		}
	}

	items, err := p.parseSelectItems()
	if err != nil {
		return nil, err
	}
	sel.Items = items

	if p.accept(token.From) {
		for {
			item, err := p.parseFromItem()
			if err != nil {
				return nil, err
			}
			sel.From = append(sel.From, item)
			if !p.accept(token.Comma) {
				break
			}
		}
	}

	if p.accept(token.Where) {
		if sel.Where, err = p.parseExpr(bpNone); err != nil {
			return nil, err
		}
	}

	if p.accept(token.Group) {
		if err := p.expect(token.By); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr(bpNone)
			if err != nil {
				return nil, err
			}
			sel.GroupBy = append(sel.GroupBy, e)
			if !p.accept(token.Comma) {
				break
			}
		}
	}

	if p.accept(token.Having) {
		if sel.Having, err = p.parseExpr(bpNone); err != nil {
			return nil, err
		}
	}

	if p.accept(token.Order) {
		if err := p.expect(token.By); err != nil {
			return nil, err
		}
		if sel.OrderBy, err = p.parseOrderBy(); err != nil {
			return nil, err
		}
	}

	// LIMIT and OFFSET may be written in either order.
	for {
		switch {
		case p.at(token.Limit) && sel.Limit == nil:
			p.next()
			if p.accept(token.All) {
				continue // LIMIT ALL means no limit.
			}
			if sel.Limit, err = p.parseExpr(bpNone); err != nil {
				return nil, err
			}
		case p.at(token.Offset) && sel.Offset == nil:
			p.next()
			if sel.Offset, err = p.parseExpr(bpNone); err != nil {
				return nil, err
			}
			// PostgreSQL allows a noise word after the count.
			if p.atWord("row") || p.atWord("rows") {
				p.next()
			}
		default:
			return sel, nil
		}
	}
}

func (p *parser) parseSelectItems() ([]ast.SelectItem, error) {
	var items []ast.SelectItem
	for {
		e, err := p.parseExpr(bpNone)
		if err != nil {
			return nil, err
		}
		item := ast.SelectItem{Expr: e}
		if alias, ok, err := p.parseAlias(); err != nil {
			return nil, err
		} else if ok {
			item.Alias = alias
		}
		items = append(items, item)
		if !p.accept(token.Comma) {
			return items, nil
		}
	}
}

// parseAlias parses an optional alias, written either with AS or as a bare
// identifier. A bare identifier is only an alias if it is not a keyword that
// starts the next clause, which is why the AS-less form is checked against the
// token kind rather than a name list.
func (p *parser) parseAlias() (ast.Name, bool, error) {
	if p.accept(token.As) {
		n, err := p.expectName()
		if err != nil {
			return ast.Name{}, false, err
		}
		return n, true, nil
	}
	if p.at(token.Ident) || p.at(token.QuotedIdent) {
		return p.name(), true, nil
	}
	return ast.Name{}, false, nil
}

func (p *parser) parseOrderBy() ([]ast.OrderByItem, error) {
	var items []ast.OrderByItem
	for {
		e, err := p.parseExpr(bpNone)
		if err != nil {
			return nil, err
		}
		item := ast.OrderByItem{Expr: e}
		switch {
		case p.accept(token.Asc):
			item.Dir = ast.SortAsc
		case p.accept(token.Desc):
			item.Dir = ast.SortDesc
		}
		if p.atWord("nulls") {
			p.next()
			switch {
			case p.atWord("first"):
				p.next()
				item.Nulls = ast.NullsFirst
			case p.atWord("last"):
				p.next()
				item.Nulls = ast.NullsLast
			default:
				return nil, p.unexpected("FIRST or LAST")
			}
		}
		items = append(items, item)
		if !p.accept(token.Comma) {
			return items, nil
		}
	}
}

// joinTypes maps the leading keyword of a join clause to its type. CROSS and the
// outer joins are recognised here; the optional OUTER and the mandatory JOIN are
// consumed by the caller.
var joinTypes = map[token.Kind]ast.JoinType{
	token.Inner: ast.InnerJoin,
	token.Left:  ast.LeftJoin,
	token.Right: ast.RightJoin,
	token.Full:  ast.FullJoin,
}

func (p *parser) parseFromItem() (ast.TableExpr, error) {
	left, err := p.parseTableFactor()
	if err != nil {
		return nil, err
	}

	for {
		joinPos := p.cur().Pos
		var typ ast.JoinType

		switch {
		case p.accept(token.Cross):
			typ = ast.CrossJoin
		case p.at(token.Join):
			typ = ast.InnerJoin
		default:
			t, ok := joinTypes[p.cur().Kind]
			if !ok {
				return left, nil
			}
			p.next()
			typ = t
			// OUTER is noise: LEFT JOIN and LEFT OUTER JOIN are the same.
			p.accept(token.Outer)
		}

		if err := p.expect(token.Join); err != nil {
			return nil, err
		}
		right, err := p.parseTableFactor()
		if err != nil {
			return nil, err
		}

		join := &ast.JoinExpr{Left: left, JoinPos: joinPos, Type: typ, Right: right}
		switch {
		case p.accept(token.On):
			if join.On, err = p.parseExpr(bpNone); err != nil {
				return nil, err
			}
		case p.accept(token.Using):
			cols, err := p.parseNameList()
			if err != nil {
				return nil, err
			}
			join.Using = cols
		case typ != ast.CrossJoin:
			return nil, p.unexpected("ON or USING")
		}
		left = join
	}
}

func (p *parser) parseTableFactor() (ast.TableExpr, error) {
	if p.at(token.LParen) {
		lparen := p.cur().Pos
		p.next()
		sel, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		if err := p.expect(token.RParen); err != nil {
			return nil, err
		}
		ref := &ast.SubqueryRef{Lparen: lparen, Select: sel}
		if alias, ok, err := p.parseAlias(); err != nil {
			return nil, err
		} else if ok {
			ref.Alias = alias
		}
		return ref, nil
	}

	name, err := p.parseTableName()
	if err != nil {
		return nil, err
	}
	ref := &ast.TableRef{Table: name}
	if alias, ok, err := p.parseAlias(); err != nil {
		return nil, err
	} else if ok {
		ref.Alias = alias
	}
	return ref, nil
}

func (p *parser) parseTableName() (*ast.TableName, error) {
	first, err := p.expectName()
	if err != nil {
		return nil, err
	}
	if !p.accept(token.Dot) {
		return &ast.TableName{Name: first}, nil
	}
	second, err := p.expectName()
	if err != nil {
		return nil, err
	}
	return &ast.TableName{Schema: first, Name: second}, nil
}

// ---------------------------------------------------------------------------
// INSERT
// ---------------------------------------------------------------------------

func (p *parser) parseInsert() (*ast.InsertStmt, error) {
	stmt := &ast.InsertStmt{InsertPos: p.cur().Pos}
	p.next()
	if err := p.expect(token.Into); err != nil {
		return nil, err
	}

	name, err := p.parseTableName()
	if err != nil {
		return nil, err
	}
	stmt.Table = name

	// A parenthesis here begins the column list. It cannot begin anything else,
	// because VALUES and SELECT are keywords.
	if p.accept(token.LParen) {
		for {
			n, err := p.expectName()
			if err != nil {
				return nil, err
			}
			stmt.Columns = append(stmt.Columns, n)
			if !p.accept(token.Comma) {
				break
			}
		}
		if err := p.expect(token.RParen); err != nil {
			return nil, err
		}
	}

	switch {
	case p.accept(token.Values):
		for {
			if err := p.expect(token.LParen); err != nil {
				return nil, err
			}
			var row []ast.Expr
			for {
				e, err := p.parseExpr(bpNone)
				if err != nil {
					return nil, err
				}
				row = append(row, e)
				if !p.accept(token.Comma) {
					break
				}
			}
			if err := p.expect(token.RParen); err != nil {
				return nil, err
			}
			stmt.Rows = append(stmt.Rows, row)
			if !p.accept(token.Comma) {
				break
			}
		}
	case p.at(token.Select):
		sel, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		stmt.Select = sel
	default:
		return nil, p.unexpected("VALUES or SELECT")
	}

	if stmt.Returning, err = p.parseReturning(); err != nil {
		return nil, err
	}
	return stmt, nil
}

// parseReturning parses an optional RETURNING list, shared by INSERT, UPDATE and
// DELETE. It reuses the select-list production, so RETURNING supports the same
// expressions and aliases a SELECT does.
func (p *parser) parseReturning() ([]ast.SelectItem, error) {
	if !p.accept(token.Returning) {
		return nil, nil
	}
	return p.parseSelectItems()
}

// ---------------------------------------------------------------------------
// UPDATE and DELETE
// ---------------------------------------------------------------------------

func (p *parser) parseUpdate() (*ast.UpdateStmt, error) {
	stmt := &ast.UpdateStmt{UpdatePos: p.cur().Pos}
	p.next()

	name, err := p.parseTableName()
	if err != nil {
		return nil, err
	}
	stmt.Table = name

	// An alias may only be introduced with AS here. Without that restriction the
	// bare-identifier form would swallow the mandatory SET keyword's position in
	// statements such as `UPDATE t x SET ...`, which PostgreSQL also rejects
	// unless AS is used.
	if p.accept(token.As) {
		if stmt.Alias, err = p.expectName(); err != nil {
			return nil, err
		}
	}

	if err := p.expect(token.Set); err != nil {
		return nil, err
	}
	for {
		col, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(token.Eq); err != nil {
			return nil, err
		}
		val, err := p.parseExpr(bpNone)
		if err != nil {
			return nil, err
		}
		stmt.Assignments = append(stmt.Assignments, &ast.Assignment{Column: col, Value: val})
		if !p.accept(token.Comma) {
			break
		}
	}

	if p.accept(token.Where) {
		if stmt.Where, err = p.parseExpr(bpNone); err != nil {
			return nil, err
		}
	}
	if stmt.Returning, err = p.parseReturning(); err != nil {
		return nil, err
	}
	return stmt, nil
}

func (p *parser) parseDelete() (*ast.DeleteStmt, error) {
	stmt := &ast.DeleteStmt{DeletePos: p.cur().Pos}
	p.next()
	if err := p.expect(token.From); err != nil {
		return nil, err
	}

	name, err := p.parseTableName()
	if err != nil {
		return nil, err
	}
	stmt.Table = name

	if p.accept(token.As) {
		if stmt.Alias, err = p.expectName(); err != nil {
			return nil, err
		}
	}

	if p.accept(token.Where) {
		if stmt.Where, err = p.parseExpr(bpNone); err != nil {
			return nil, err
		}
	}
	if stmt.Returning, err = p.parseReturning(); err != nil {
		return nil, err
	}
	return stmt, nil
}

// ---------------------------------------------------------------------------
// CREATE TABLE
// ---------------------------------------------------------------------------

func (p *parser) parseCreate() (ast.Stmt, error) {
	createPos := p.cur().Pos
	p.next()
	if err := p.expect(token.Table); err != nil {
		return nil, err
	}

	stmt := &ast.CreateTableStmt{CreatePos: createPos}
	if p.accept(token.If) {
		if err := p.expect(token.Not); err != nil {
			return nil, err
		}
		if err := p.expect(token.Exists); err != nil {
			return nil, err
		}
		stmt.IfNotExists = true
	}

	name, err := p.parseTableName()
	if err != nil {
		return nil, err
	}
	stmt.Table = name

	if err := p.expect(token.LParen); err != nil {
		return nil, err
	}
	for {
		if err := p.parseTableElement(stmt); err != nil {
			return nil, err
		}
		if !p.accept(token.Comma) {
			break
		}
	}
	if err := p.expect(token.RParen); err != nil {
		return nil, err
	}
	if len(stmt.Columns) == 0 {
		return nil, pgerr.Syntaxf(stmt.Table.Pos(), "table must have at least one column")
	}
	return stmt, nil
}

// parseTableElement parses one entry of a CREATE TABLE body, which is either a
// column definition or a table-level constraint. They are told apart by their
// leading keyword: only a constraint can start with CONSTRAINT, PRIMARY, UNIQUE,
// CHECK or FOREIGN.
func (p *parser) parseTableElement(stmt *ast.CreateTableStmt) error {
	pos := p.cur().Pos
	var cname ast.Name
	if p.accept(token.Constraint) {
		n, err := p.expectName()
		if err != nil {
			return err
		}
		cname = n
	}

	switch p.cur().Kind {
	case token.Primary, token.Unique, token.Check, token.Foreign:
		c, err := p.parseTableConstraint(pos, cname)
		if err != nil {
			return err
		}
		stmt.Constraints = append(stmt.Constraints, c)
		return nil
	}

	if !cname.IsEmpty() {
		return p.unexpected("PRIMARY KEY, UNIQUE, CHECK or FOREIGN KEY")
	}

	col, err := p.parseColumnDef()
	if err != nil {
		return err
	}
	stmt.Columns = append(stmt.Columns, col)
	return nil
}

func (p *parser) parseTableConstraint(pos token.Pos, name ast.Name) (*ast.TableConstraint, error) {
	c := &ast.TableConstraint{ConstraintPos: pos, Name: name}

	switch {
	case p.accept(token.Primary):
		if err := p.expect(token.Key); err != nil {
			return nil, err
		}
		c.Kind = ast.ConstraintPrimaryKey
	case p.accept(token.Unique):
		c.Kind = ast.ConstraintUnique
	case p.accept(token.Check):
		c.Kind = ast.ConstraintCheck
		expr, err := p.parseParenExpr()
		if err != nil {
			return nil, err
		}
		c.Expr = expr
		return c, nil
	default: // FOREIGN KEY
		p.next()
		if err := p.expect(token.Key); err != nil {
			return nil, err
		}
		c.Kind = ast.ConstraintReferences
	}

	cols, err := p.parseNameList()
	if err != nil {
		return nil, err
	}
	c.Columns = cols

	if c.Kind == ast.ConstraintReferences {
		if err := p.expect(token.References); err != nil {
			return nil, err
		}
		ref, err := p.parseForeignKeyRef()
		if err != nil {
			return nil, err
		}
		c.Ref = ref
	}
	return c, nil
}

func (p *parser) parseColumnDef() (*ast.ColumnDef, error) {
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	typ, err := p.parseTypeName()
	if err != nil {
		return nil, err
	}
	col := &ast.ColumnDef{Name: name, Type: typ}

	for {
		pos := p.cur().Pos
		var cname ast.Name
		if p.accept(token.Constraint) {
			n, err := p.expectName()
			if err != nil {
				return nil, err
			}
			cname = n
		}

		c := &ast.ColumnConstraint{ConstraintPos: pos, Name: cname}
		switch {
		case p.accept(token.Not):
			if err := p.expect(token.Null); err != nil {
				return nil, err
			}
			c.Kind = ast.ConstraintNotNull
		case p.accept(token.Null):
			c.Kind = ast.ConstraintNull
		case p.accept(token.Primary):
			if err := p.expect(token.Key); err != nil {
				return nil, err
			}
			c.Kind = ast.ConstraintPrimaryKey
		case p.accept(token.Unique):
			c.Kind = ast.ConstraintUnique
		case p.accept(token.Default):
			c.Kind = ast.ConstraintDefault
			if c.Expr, err = p.parseExpr(bpNone); err != nil {
				return nil, err
			}
		case p.accept(token.Check):
			c.Kind = ast.ConstraintCheck
			if c.Expr, err = p.parseParenExpr(); err != nil {
				return nil, err
			}
		case p.accept(token.References):
			c.Kind = ast.ConstraintReferences
			if c.Ref, err = p.parseForeignKeyRef(); err != nil {
				return nil, err
			}
		default:
			if !cname.IsEmpty() {
				return nil, p.unexpected("a constraint")
			}
			return col, nil
		}
		col.Constraints = append(col.Constraints, c)
	}
}

func (p *parser) parseForeignKeyRef() (*ast.ForeignKeyRef, error) {
	table, err := p.parseTableName()
	if err != nil {
		return nil, err
	}
	ref := &ast.ForeignKeyRef{Table: table}

	if p.at(token.LParen) {
		cols, err := p.parseNameList()
		if err != nil {
			return nil, err
		}
		ref.Columns = cols
	}

	for p.accept(token.On) {
		switch {
		case p.accept(token.Delete):
			if ref.OnDelete, err = p.parseRefAction(); err != nil {
				return nil, err
			}
		case p.accept(token.Update):
			if ref.OnUpdate, err = p.parseRefAction(); err != nil {
				return nil, err
			}
		default:
			return nil, p.unexpected("DELETE or UPDATE")
		}
	}
	return ref, nil
}

func (p *parser) parseRefAction() (ast.RefAction, error) {
	switch {
	case p.atWord("cascade"):
		p.next()
		return ast.Cascade, nil
	case p.atWord("restrict"):
		p.next()
		return ast.Restrict, nil
	case p.atWord("no"):
		p.next()
		if err := p.expectWord("action"); err != nil {
			return 0, err
		}
		return ast.NoAction, nil
	case p.accept(token.Set):
		switch {
		case p.accept(token.Null):
			return ast.SetNull, nil
		case p.accept(token.Default):
			return ast.SetDefault, nil
		}
		return 0, p.unexpected("NULL or DEFAULT")
	}
	return 0, p.unexpected("a referential action")
}

func (p *parser) parseNameList() ([]ast.Name, error) {
	if err := p.expect(token.LParen); err != nil {
		return nil, err
	}
	var names []ast.Name
	for {
		n, err := p.expectName()
		if err != nil {
			return nil, err
		}
		names = append(names, n)
		if !p.accept(token.Comma) {
			break
		}
	}
	if err := p.expect(token.RParen); err != nil {
		return nil, err
	}
	return names, nil
}

func (p *parser) parseParenExpr() (ast.Expr, error) {
	if err := p.expect(token.LParen); err != nil {
		return nil, err
	}
	e, err := p.parseExpr(bpNone)
	if err != nil {
		return nil, err
	}
	if err := p.expect(token.RParen); err != nil {
		return nil, err
	}
	return e, nil
}
