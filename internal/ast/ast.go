// Package ast defines lightsql's abstract syntax tree.
//
// Every construct has its own node type, shaped after go/ast. The alternative —
// one generic node carrying a token, a lexeme and a slice of children — pushes
// the grammar out of the type system and into positional child access, so that
// reading a join condition becomes on.Decl[0].Decl[0].Lexeme and the tree's shape
// is documented only in comments. Typed nodes make the shape checkable, make
// exhaustive switches possible, and make an out-of-range child a compile error
// rather than a panic.
//
// Two invariants hold throughout:
//
//   - Every node reports a source position, so any error raised while walking
//     the tree can point at the offending text.
//   - The tree is immutable once parsed. Nothing downstream rewrites or consumes
//     nodes in place, which is what allows a prepared statement to be planned
//     once and executed many times.
package ast

import "github.com/oxisto/lightsql/internal/token"

// Node is the interface implemented by all AST nodes.
type Node interface {
	// Pos returns the position of the node's first token.
	Pos() token.Pos
}

// Stmt is a SQL statement.
type Stmt interface {
	Node
	stmtNode()
}

// Expr is a scalar expression.
type Expr interface {
	Node
	exprNode()
}

// TableExpr is anything that can appear in a FROM clause and produce rows.
type TableExpr interface {
	Node
	tableExprNode()
}

// ---------------------------------------------------------------------------
// Names
// ---------------------------------------------------------------------------

// Name is an identifier occurrence. Quoted records whether it was written as a
// delimited identifier, which matters because an unquoted name has already been
// folded to lower case and a quoted one has not.
type Name struct {
	NamePos token.Pos
	Name    string
	Quoted  bool
}

func (n Name) Pos() token.Pos { return n.NamePos }

// IsEmpty reports whether the name is absent, as for an omitted alias.
func (n Name) IsEmpty() bool { return n.Name == "" }

// TableName is a possibly schema-qualified table name.
type TableName struct {
	// Schema is the zero Name when the table was written unqualified.
	Schema Name
	Name   Name
}

func (t *TableName) Pos() token.Pos {
	if !t.Schema.IsEmpty() {
		return t.Schema.NamePos
	}
	return t.Name.NamePos
}

// String renders the name as written, for error messages.
func (t *TableName) String() string {
	if t.Schema.IsEmpty() {
		return t.Name.Name
	}
	return t.Schema.Name + "." + t.Name.Name
}

// ---------------------------------------------------------------------------
// Expressions
// ---------------------------------------------------------------------------

// LiteralKind classifies a literal token.
type LiteralKind uint8

const (
	// LitString is a quoted string whose escapes the scanner already resolved.
	LitString LiteralKind = iota
	// LitNumber holds the literal's source text; the binder decides whether it
	// becomes an integer or a float, because that depends on the target type.
	LitNumber
	LitTrue
	LitFalse
	LitNull
)

// Literal is a constant appearing in the statement text.
type Literal struct {
	ValuePos token.Pos
	Kind     LiteralKind
	// Val is the semantic text: the resolved string contents, or the numeric
	// source text. It is empty for the keyword literals.
	Val string
}

// Param is a $1 or ? placeholder. Ord is the 1-based ordinal, resolved by the
// scanner rather than by a counter mutated during a later tree walk.
type Param struct {
	ParamPos token.Pos
	Ord      int
}

// ColumnRef is a reference to a column, optionally qualified by table and
// schema. The binder resolves it to a column ordinal; nothing downstream of the
// binder compares column names as strings.
type ColumnRef struct {
	// Schema and Table are zero Names when omitted.
	Schema Name
	Table  Name
	Column Name
}

// Star is the * in SELECT *, optionally qualified as t.*.
type Star struct {
	StarPos token.Pos
	Table   Name // zero when unqualified
}

// BinaryOp identifies a binary operator.
type BinaryOp uint8

const (
	OpAdd BinaryOp = iota
	OpSub
	OpMul
	OpDiv
	OpMod
	OpExp
	OpConcat
	OpEq
	OpNe
	OpLt
	OpLe
	OpGt
	OpGe
	OpAnd
	OpOr
	OpLike
	OpNotLike
	OpIsDistinctFrom
	OpIsNotDistinctFrom
	OpJSONField
	OpJSONText
	OpJSONContains
)

var binaryOpNames = [...]string{
	OpAdd: "+", OpSub: "-", OpMul: "*", OpDiv: "/", OpMod: "%", OpExp: "^",
	OpConcat: "||", OpEq: "=", OpNe: "<>", OpLt: "<", OpLe: "<=", OpGt: ">",
	OpGe: ">=", OpAnd: "AND", OpOr: "OR", OpLike: "LIKE", OpNotLike: "NOT LIKE",
	OpIsDistinctFrom: "IS DISTINCT FROM", OpIsNotDistinctFrom: "IS NOT DISTINCT FROM",
	OpJSONField: "->", OpJSONText: "->>", OpJSONContains: "@>",
}

func (o BinaryOp) String() string { return binaryOpNames[o] }

// IsComparison reports whether the operator yields a boolean from two operands
// of comparable type.
func (o BinaryOp) IsComparison() bool { return o >= OpEq && o <= OpGe }

// BinaryExpr is `X Op Y`. Nesting is explicit, so precedence and associativity
// are decided once by the parser and are visible in the tree rather than being
// re-derived by every consumer.
type BinaryExpr struct {
	Op    BinaryOp
	OpPos token.Pos
	X, Y  Expr
}

// UnaryOp identifies a prefix operator.
type UnaryOp uint8

const (
	OpNeg UnaryOp = iota
	OpPlus
	OpNot
)

var unaryOpNames = [...]string{OpNeg: "-", OpPlus: "+", OpNot: "NOT"}

func (o UnaryOp) String() string { return unaryOpNames[o] }

// UnaryExpr is `Op X`.
type UnaryExpr struct {
	Op    UnaryOp
	OpPos token.Pos
	X     Expr
}

// ParenExpr is `( X )`. It is kept in the tree so that the printer can round
// trip, and so error positions point where the user wrote them.
type ParenExpr struct {
	Lparen token.Pos
	X      Expr
}

// IsNullExpr is `X IS NULL` or `X IS NOT NULL`. It is a distinct node rather
// than a comparison against a NULL literal because it does not follow
// three-valued comparison rules: it always yields TRUE or FALSE.
type IsNullExpr struct {
	X      Expr
	IsPos  token.Pos
	Negate bool
}

// CastExpr is `CAST(X AS Type)` or `X::Type`.
type CastExpr struct {
	CastPos token.Pos
	X       Expr
	Type    *TypeName
}

// FuncCall is a function or aggregate call.
type FuncCall struct {
	Name   Name
	Lparen token.Pos
	Args   []Expr
	// Star records COUNT(*), which takes no argument expression.
	Star bool
	// Distinct records COUNT(DISTINCT x).
	Distinct bool
}

// InExpr is `X IN (...)`. Exactly one of List and Subquery is set.
type InExpr struct {
	X        Expr
	InPos    token.Pos
	Negate   bool
	List     []Expr
	Subquery *SelectStmt
}

// BetweenExpr is `X BETWEEN Lo AND Hi`.
type BetweenExpr struct {
	X          Expr
	BetweenPos token.Pos
	Negate     bool
	Lo, Hi     Expr
}

// CaseExpr is a CASE expression. When Operand is nil the form is the searched
// CASE, where each When condition is a full predicate.
type CaseExpr struct {
	CasePos token.Pos
	Operand Expr
	Whens   []*WhenClause
	Else    Expr
}

// WhenClause is one WHEN/THEN arm of a CASE expression.
type WhenClause struct {
	WhenPos     token.Pos
	Cond, Value Expr
}

func (w *WhenClause) Pos() token.Pos { return w.WhenPos }

// ExistsExpr is `EXISTS (subquery)`.
type ExistsExpr struct {
	ExistsPos token.Pos
	Negate    bool
	Subquery  *SelectStmt
}

// SubqueryExpr is a scalar subquery used in expression position.
type SubqueryExpr struct {
	Lparen token.Pos
	Select *SelectStmt
}

func (e *Literal) Pos() token.Pos      { return e.ValuePos }
func (e *Param) Pos() token.Pos        { return e.ParamPos }
func (e *ColumnRef) Pos() token.Pos    { return e.first().NamePos }
func (e *Star) Pos() token.Pos         { return e.StarPos }
func (e *BinaryExpr) Pos() token.Pos   { return e.X.Pos() }
func (e *UnaryExpr) Pos() token.Pos    { return e.OpPos }
func (e *ParenExpr) Pos() token.Pos    { return e.Lparen }
func (e *IsNullExpr) Pos() token.Pos   { return e.X.Pos() }
func (e *CastExpr) Pos() token.Pos     { return e.CastPos }
func (e *FuncCall) Pos() token.Pos     { return e.Name.NamePos }
func (e *InExpr) Pos() token.Pos       { return e.X.Pos() }
func (e *BetweenExpr) Pos() token.Pos  { return e.X.Pos() }
func (e *CaseExpr) Pos() token.Pos     { return e.CasePos }
func (e *ExistsExpr) Pos() token.Pos   { return e.ExistsPos }
func (e *SubqueryExpr) Pos() token.Pos { return e.Lparen }

// first returns the leftmost written component of a column reference.
func (e *ColumnRef) first() Name {
	if !e.Schema.IsEmpty() {
		return e.Schema
	}
	if !e.Table.IsEmpty() {
		return e.Table
	}
	return e.Column
}

// String renders the reference as written, for error messages.
func (e *ColumnRef) String() string {
	s := e.Column.Name
	if !e.Table.IsEmpty() {
		s = e.Table.Name + "." + s
	}
	if !e.Schema.IsEmpty() {
		s = e.Schema.Name + "." + s
	}
	return s
}

func (*Literal) exprNode()      {}
func (*Param) exprNode()        {}
func (*ColumnRef) exprNode()    {}
func (*Star) exprNode()         {}
func (*BinaryExpr) exprNode()   {}
func (*UnaryExpr) exprNode()    {}
func (*ParenExpr) exprNode()    {}
func (*IsNullExpr) exprNode()   {}
func (*CastExpr) exprNode()     {}
func (*FuncCall) exprNode()     {}
func (*InExpr) exprNode()       {}
func (*BetweenExpr) exprNode()  {}
func (*CaseExpr) exprNode()     {}
func (*ExistsExpr) exprNode()   {}
func (*SubqueryExpr) exprNode() {}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// TypeName is a written type, such as `varchar(255)` or `numeric(10, 2)`.
// Resolution to a types.Kind happens in the binder, which owns the dialect's
// type aliases.
type TypeName struct {
	NamePos token.Pos
	// Name is the folded type name, with any multi-word spelling normalised to
	// a single space, so that "double precision" arrives as written.
	Name string
	// Mods holds the parenthesised modifiers, e.g. the 255 of varchar(255).
	Mods []int
}

func (t *TypeName) Pos() token.Pos { return t.NamePos }

// ---------------------------------------------------------------------------
// FROM clause
// ---------------------------------------------------------------------------

// TableRef is a base table in a FROM clause, with an optional alias.
type TableRef struct {
	Table *TableName
	Alias Name
}

// SubqueryRef is a parenthesised subquery in a FROM clause.
type SubqueryRef struct {
	Lparen token.Pos
	Select *SelectStmt
	Alias  Name
}

// JoinType identifies the join flavour.
type JoinType uint8

const (
	InnerJoin JoinType = iota
	LeftJoin
	RightJoin
	FullJoin
	CrossJoin
)

var joinTypeNames = [...]string{
	InnerJoin: "INNER", LeftJoin: "LEFT", RightJoin: "RIGHT",
	FullJoin: "FULL", CrossJoin: "CROSS",
}

func (j JoinType) String() string { return joinTypeNames[j] }

// JoinExpr is a join of two table expressions. Exactly one of On and Using is
// set, except for a CROSS JOIN where neither is.
type JoinExpr struct {
	Left    TableExpr
	JoinPos token.Pos
	Type    JoinType
	Right   TableExpr
	On      Expr
	Using   []Name
}

func (t *TableRef) Pos() token.Pos    { return t.Table.Pos() }
func (t *SubqueryRef) Pos() token.Pos { return t.Lparen }
func (t *JoinExpr) Pos() token.Pos    { return t.Left.Pos() }

func (*TableRef) tableExprNode()    {}
func (*SubqueryRef) tableExprNode() {}
func (*JoinExpr) tableExprNode()    {}

// ---------------------------------------------------------------------------
// SELECT
// ---------------------------------------------------------------------------

// SelectItem is one entry of a select list.
type SelectItem struct {
	Expr  Expr
	Alias Name
}

// SortDir is the direction of an ORDER BY term.
type SortDir uint8

const (
	SortAsc SortDir = iota
	SortDesc
)

// NullsOrder controls where NULLs sort relative to other values.
type NullsOrder uint8

const (
	// NullsDefault means NULLS LAST for ASC and NULLS FIRST for DESC, matching
	// PostgreSQL's rule that NULLs sort as the largest value.
	NullsDefault NullsOrder = iota
	NullsFirst
	NullsLast
)

// OrderByItem is one term of an ORDER BY clause.
type OrderByItem struct {
	Expr  Expr
	Dir   SortDir
	Nulls NullsOrder
}

// SelectStmt is a SELECT query.
type SelectStmt struct {
	SelectPos token.Pos
	Distinct  bool
	// DistinctOn holds the expressions of DISTINCT ON (...); it is non-empty
	// only when Distinct is true.
	DistinctOn []Expr
	Items      []SelectItem
	From       []TableExpr
	Where      Expr
	GroupBy    []Expr
	Having     Expr
	OrderBy    []OrderByItem
	Limit      Expr
	Offset     Expr
}

func (s *SelectStmt) Pos() token.Pos { return s.SelectPos }

// ---------------------------------------------------------------------------
// INSERT
// ---------------------------------------------------------------------------

// InsertStmt is an INSERT statement. Exactly one of Rows and Select is set.
type InsertStmt struct {
	InsertPos token.Pos
	Table     *TableName
	// Columns is empty when the statement omits the column list, meaning all
	// columns in declaration order.
	Columns   []Name
	Rows      [][]Expr
	Select    *SelectStmt
	Returning []SelectItem
}

func (s *InsertStmt) Pos() token.Pos { return s.InsertPos }

// ---------------------------------------------------------------------------
// UPDATE and DELETE
// ---------------------------------------------------------------------------

// Assignment is one `column = expression` of an UPDATE SET clause.
type Assignment struct {
	Column Name
	Value  Expr
}

func (a *Assignment) Pos() token.Pos { return a.Column.NamePos }

// UpdateStmt is an UPDATE statement.
type UpdateStmt struct {
	UpdatePos token.Pos
	Table     *TableName
	// Alias renames the table for the rest of the statement, as in
	// `UPDATE t AS x SET ... WHERE x.a = 1`.
	Alias       Name
	Assignments []*Assignment
	Where       Expr
	Returning   []SelectItem
}

func (s *UpdateStmt) Pos() token.Pos { return s.UpdatePos }

// DeleteStmt is a DELETE statement.
type DeleteStmt struct {
	DeletePos token.Pos
	Table     *TableName
	Alias     Name
	Where     Expr
	Returning []SelectItem
}

func (s *DeleteStmt) Pos() token.Pos { return s.DeletePos }

// ---------------------------------------------------------------------------
// CREATE TABLE
// ---------------------------------------------------------------------------

// ConstraintKind identifies a column or table constraint.
//
// Constraints use one tagged node rather than a type per kind because they form
// a small closed set with a uniform shape. That is a different situation from
// the tree at large, where a single generic node would erase the grammar.
type ConstraintKind uint8

const (
	ConstraintNotNull ConstraintKind = iota
	ConstraintNull
	ConstraintPrimaryKey
	ConstraintUnique
	ConstraintDefault
	ConstraintCheck
	ConstraintReferences
)

var constraintKindNames = [...]string{
	ConstraintNotNull: "NOT NULL", ConstraintNull: "NULL",
	ConstraintPrimaryKey: "PRIMARY KEY", ConstraintUnique: "UNIQUE",
	ConstraintDefault: "DEFAULT", ConstraintCheck: "CHECK",
	ConstraintReferences: "REFERENCES",
}

func (k ConstraintKind) String() string { return constraintKindNames[k] }

// ColumnConstraint is a constraint written as part of a column definition.
type ColumnConstraint struct {
	ConstraintPos token.Pos
	Kind          ConstraintKind
	// Name is the CONSTRAINT name, zero when unnamed.
	Name Name
	// Expr carries the DEFAULT value or the CHECK predicate.
	Expr Expr
	// Ref carries the REFERENCES target.
	Ref *ForeignKeyRef
}

func (c *ColumnConstraint) Pos() token.Pos { return c.ConstraintPos }

// ForeignKeyRef is the target of a REFERENCES clause.
type ForeignKeyRef struct {
	Table *TableName
	// Columns is empty when the reference targets the primary key.
	Columns  []Name
	OnDelete RefAction
	OnUpdate RefAction
}

// RefAction is the referential action of a foreign key.
type RefAction uint8

const (
	NoAction RefAction = iota
	Restrict
	Cascade
	SetNull
	SetDefault
)

var refActionNames = [...]string{
	NoAction: "NO ACTION", Restrict: "RESTRICT", Cascade: "CASCADE",
	SetNull: "SET NULL", SetDefault: "SET DEFAULT",
}

func (a RefAction) String() string { return refActionNames[a] }

// ColumnDef is one column of a CREATE TABLE statement.
type ColumnDef struct {
	Name        Name
	Type        *TypeName
	Constraints []*ColumnConstraint
}

func (c *ColumnDef) Pos() token.Pos { return c.Name.NamePos }

// TableConstraint is a constraint written at table level, over one or more
// columns.
type TableConstraint struct {
	ConstraintPos token.Pos
	Kind          ConstraintKind
	Name          Name
	Columns       []Name
	Expr          Expr
	Ref           *ForeignKeyRef
}

func (c *TableConstraint) Pos() token.Pos { return c.ConstraintPos }

// CreateTableStmt is a CREATE TABLE statement.
type CreateTableStmt struct {
	CreatePos   token.Pos
	IfNotExists bool
	Table       *TableName
	Columns     []*ColumnDef
	Constraints []*TableConstraint
}

func (s *CreateTableStmt) Pos() token.Pos { return s.CreatePos }

// AlterTableStmt is ALTER TABLE.
//
// The action is a typed node rather than a set of optional fields, because
// ALTER TABLE is a family of unrelated statements sharing a prefix: what RENAME
// TO carries has nothing to do with what ADD COLUMN would. Fields for both would
// make every consumer check which of them is set.
type AlterTableStmt struct {
	AlterPos token.Pos
	Table    *TableName
	Action   AlterAction
}

func (s *AlterTableStmt) Pos() token.Pos { return s.AlterPos }

// AlterAction is one thing ALTER TABLE can do.
type AlterAction interface {
	Node
	alterActionNode()
}

// RenameTable is ALTER TABLE ... RENAME TO.
type RenameTable struct {
	RenamePos token.Pos
	To        Name
}

// RenameColumn is ALTER TABLE ... RENAME COLUMN ... TO.
type RenameColumn struct {
	RenamePos token.Pos
	From, To  Name
}

func (a *RenameTable) Pos() token.Pos  { return a.RenamePos }
func (a *RenameColumn) Pos() token.Pos { return a.RenamePos }

func (*RenameTable) alterActionNode()  {}
func (*RenameColumn) alterActionNode() {}

// CreateIndexStmt is CREATE INDEX.
//
// Where is the predicate of a partial index, or nil. A partial index covers
// only the rows it selects, so a unique one constrains only those.
type CreateIndexStmt struct {
	CreatePos   token.Pos
	Unique      bool
	IfNotExists bool
	Name        Name
	Table       *TableName
	Columns     []Name
	Where       Expr
}

func (s *CreateIndexStmt) Pos() token.Pos { return s.CreatePos }

// DropIndexStmt is DROP INDEX.
type DropIndexStmt struct {
	DropPos  token.Pos
	IfExists bool
	Names    []Name
}

func (s *DropIndexStmt) Pos() token.Pos { return s.DropPos }

// DropTableStmt is DROP TABLE, which may name several tables at once.
//
// Cascade records whether CASCADE was written. The default is RESTRICT, so a
// table another one references is kept rather than silently taking its
// referencing constraints with it.
type DropTableStmt struct {
	DropPos  token.Pos
	IfExists bool
	Tables   []*TableName
	Cascade  bool
}

func (s *DropTableStmt) Pos() token.Pos { return s.DropPos }

func (*AlterTableStmt) stmtNode()  {}
func (*CreateIndexStmt) stmtNode() {}
func (*DropIndexStmt) stmtNode()   {}
func (*DropTableStmt) stmtNode()   {}
func (*SelectStmt) stmtNode()      {}
func (*InsertStmt) stmtNode()      {}
func (*UpdateStmt) stmtNode()      {}
func (*DeleteStmt) stmtNode()      {}
func (*CreateTableStmt) stmtNode() {}

// ReferencesColumn reports whether an expression mentions the given unqualified
// column name.
//
// The catalog stores CHECK predicates, DEFAULT expressions and partial index
// predicates as syntax, so a column rename has to know whether any of them would
// be left naming a column that no longer exists.
//
// The switch is exhaustive over the expression nodes rather than reflective, so
// adding a node that can contain a column reference is a compile-time prompt to
// handle it here.
func ReferencesColumn(e Expr, name string) bool {
	switch e := e.(type) {
	case nil:
		return false
	case *ColumnRef:
		return e.Column.Name == name
	case *ParenExpr:
		return ReferencesColumn(e.X, name)
	case *UnaryExpr:
		return ReferencesColumn(e.X, name)
	case *BinaryExpr:
		return ReferencesColumn(e.X, name) || ReferencesColumn(e.Y, name)
	case *IsNullExpr:
		return ReferencesColumn(e.X, name)
	case *CastExpr:
		return ReferencesColumn(e.X, name)
	case *FuncCall:
		return anyReferences(e.Args, name)
	case *InExpr:
		return ReferencesColumn(e.X, name) || anyReferences(e.List, name)
	case *BetweenExpr:
		return ReferencesColumn(e.X, name) ||
			ReferencesColumn(e.Lo, name) || ReferencesColumn(e.Hi, name)
	case *CaseExpr:
		if ReferencesColumn(e.Operand, name) || ReferencesColumn(e.Else, name) {
			return true
		}
		for _, w := range e.Whens {
			if ReferencesColumn(w.Cond, name) || ReferencesColumn(w.Value, name) {
				return true
			}
		}
		return false
	default:
		// A literal, a parameter or a star mentions no column. A subquery is
		// deliberately not descended into: one cannot appear in a CHECK, a
		// DEFAULT or an index predicate, which are the only expressions the
		// catalog stores.
		return false
	}
}

func anyReferences(xs []Expr, name string) bool {
	for _, x := range xs {
		if ReferencesColumn(x, name) {
			return true
		}
	}
	return false
}
