// Package plan defines the bound representation of a statement: the form the
// binder produces and the executor consumes.
//
// The essential difference from the AST is that names are gone. A column
// reference is an ordinal, a type is resolved, and a function is chosen. Nothing
// downstream compares a string to find a column, which is both the correctness
// argument (a rename cannot silently match the wrong column) and the performance
// one (the alternative is a string comparison, and often a concatenation, for
// every column of every row).
//
// Keeping this separate from the executor's operators also gives the optimizer
// something to rewrite: predicate pushdown and index selection are
// transformations on this tree.
package plan

import (
	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/types"
)

// Expr is a bound scalar expression.
type Expr interface {
	// Type is the kind the expression evaluates to. It is known statically,
	// because the binder resolved it.
	Type() types.Kind
	exprNode()
}

// Column reads a column of the current row by ordinal.
type Column struct {
	Ordinal int
	Kind    types.Kind
	// Name is retained only for error messages and EXPLAIN output.
	Name string
}

// Const is a literal whose value was computed during binding.
type Const struct {
	Val types.Value
}

// Param is a placeholder, resolved against the arguments supplied at execution.
type Param struct {
	// Ord is the 1-based ordinal assigned by the scanner.
	Ord  int
	Kind types.Kind
}

// Binary is a binary operation on two bound operands.
type Binary struct {
	Op   ast.BinaryOp
	L, R Expr
	Kind types.Kind
}

// Unary is a prefix operation.
type Unary struct {
	Op   ast.UnaryOp
	X    Expr
	Kind types.Kind
}

// IsNull is IS NULL or IS NOT NULL. It stays distinct from a comparison because
// it always yields true or false, never unknown.
type IsNull struct {
	X      Expr
	Negate bool
}

func (e *Column) Type() types.Kind { return e.Kind }
func (e *Const) Type() types.Kind  { return e.Val.Kind() }
func (e *Param) Type() types.Kind  { return e.Kind }
func (e *Binary) Type() types.Kind { return e.Kind }
func (e *Unary) Type() types.Kind  { return e.Kind }
func (e *IsNull) Type() types.Kind { return types.KindBool }

func (*Column) exprNode() {}
func (*Const) exprNode()  {}
func (*Param) exprNode()  {}
func (*Binary) exprNode() {}
func (*Unary) exprNode()  {}
func (*IsNull) exprNode() {}

// ResultColumn describes one column of a node's output. The driver reports these
// through the RowsColumnType interfaces, which is how an ORM decides what Go
// type to scan into.
type ResultColumn struct {
	Name string
	Type catalog.Type
}

// Node is a bound relational operator.
type Node interface {
	// Result describes the columns this node produces.
	Result() []ResultColumn
	planNode()
}

// SingleRow produces exactly one row with no columns.
//
// It is what a SELECT without FROM is evaluated over, and it exists so that
// every other node always has an input. Representing "no FROM clause" as a nil
// input instead means each consumer has to remember the special case, which is
// how `SELECT 1 ORDER BY 1` — valid SQL — ended up rejected while
// `SELECT 1` worked.
type SingleRow struct{}

// Scan reads every row of a table.
type Scan struct {
	Table *catalog.Table
	// Cols mirrors the table's columns, resolved once so the executor does not
	// consult the catalog per row.
	Cols []ResultColumn
}

// Filter keeps the rows for which Pred evaluates to true. Rows for which the
// predicate is false or unknown are both dropped, which is SQL's rule.
type Filter struct {
	Input Node
	Pred  Expr
}

// Project evaluates a list of expressions over each input row.
type Project struct {
	Input Node
	Exprs []Expr
	Cols  []ResultColumn
}

// SortKey is one ORDER BY term.
//
// NullsFirst is resolved here rather than carried as a three-state default,
// because the default depends on the direction: PostgreSQL sorts NULL as the
// largest value, so ASC puts NULLs last and DESC puts them first. Leaving that
// to the executor would mean re-deriving it per comparison.
type SortKey struct {
	Expr       Expr
	Desc       bool
	NullsFirst bool
}

// Sort orders its input.
//
// It sits below Project, not above it, because ORDER BY may name a column that
// the select list does not output — `SELECT a FROM t ORDER BY b` is valid SQL.
// Its keys are therefore bound in the input's scope. A term that names an output
// alias is resolved by reusing that select item's bound expression, which is an
// input-scope expression too, so both forms land in the same place.
type Sort struct {
	Input Node
	Keys  []SortKey
}

// Limit restricts the row count. Count is nil when only OFFSET was given.
type Limit struct {
	Input         Node
	Count, Offset Expr
}

func (n *SingleRow) Result() []ResultColumn { return nil }
func (n *Scan) Result() []ResultColumn      { return n.Cols }
func (n *Filter) Result() []ResultColumn    { return n.Input.Result() }
func (n *Project) Result() []ResultColumn   { return n.Cols }
func (n *Sort) Result() []ResultColumn      { return n.Input.Result() }
func (n *Limit) Result() []ResultColumn     { return n.Input.Result() }

func (*SingleRow) planNode() {}
func (*Scan) planNode()      {}
func (*Filter) planNode()    {}
func (*Project) planNode()   {}
func (*Sort) planNode()      {}
func (*Limit) planNode()     {}

// Stmt is a bound statement.
type Stmt interface {
	stmtNode()
}

// CreateTable creates a table from an already validated definition.
type CreateTable struct {
	Table       *catalog.Table
	IfNotExists bool
}

// Insert adds rows to a table.
type Insert struct {
	Table *catalog.Table
	// Targets maps each expression position to a column ordinal, so the
	// executor never re-derives the column order.
	Targets []int
	Rows    [][]Expr
	// Serials lists ordinals of serial columns that the statement omitted and
	// which must therefore be filled from the column's sequence.
	Serials []int
	// Defaults holds the bound DEFAULT expression for each omitted column that
	// has one, keyed by ordinal.
	Defaults map[int]Expr
	// Checks are the table's CHECK constraints, bound for this statement.
	Checks []Check
	// Returning is nil unless the statement had a RETURNING clause.
	Returning *Returning
}

// Check is a bound CHECK constraint.
//
// A check passes when its predicate is true OR unknown, and fails only on
// false. That is the opposite of a WHERE clause, which keeps only true, and it
// is why a NULL column does not trip a check written about it.
type Check struct {
	Name string
	Pred Expr
}

// Assignment sets one column of a row being updated.
type Assignment struct {
	// Ordinal is the column's position, resolved by the binder.
	Ordinal int
	Value   Expr
}

// Update modifies rows matching Where.
//
// Unlike a query, this is not built on a Scan node. Row modification needs to
// identify the storage slot a row occupies, which the row itself does not carry,
// so the executor walks the table directly. Once indexes exist and the optimizer
// can choose an access path, this becomes a node over a scan.
type Update struct {
	Table       *catalog.Table
	Where       Expr // nil means every row
	Assignments []Assignment
	Checks      []Check
	Returning   *Returning
}

// Delete removes rows matching Where.
type Delete struct {
	Table     *catalog.Table
	Where     Expr // nil means every row
	Returning *Returning
}

// Returning describes a RETURNING clause. It is shared by INSERT, UPDATE and
// DELETE, all of which may produce rows as well as a count.
type Returning struct {
	Exprs []Expr
	Cols  []ResultColumn
}

// Query is a statement that returns rows.
type Query struct {
	Root Node
}

func (*CreateTable) stmtNode() {}
func (*Insert) stmtNode()      {}
func (*Update) stmtNode()      {}
func (*Delete) stmtNode()      {}
func (*Query) stmtNode()       {}
