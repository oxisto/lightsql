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

// Cast is an explicit conversion, from CAST(x AS t) or x::t. Only conversions
// that cannot be folded at bind time survive to here; a cast of a constant is
// performed by the binder so the cost is not paid per row.
type Cast struct {
	X    Expr
	Kind types.Kind
}

// ScalarSubquery is a SELECT used where a single value is expected.
//
// It yields NULL when the subquery returns no rows, and raises a cardinality
// violation when it returns more than one. Picking the first would make the
// answer depend on a row order the query never asked for.
type ScalarSubquery struct {
	Input Node
	Kind  types.Kind
}

// ExistsSubquery is EXISTS (SELECT ...), or NOT EXISTS when Negate is set.
//
// It is the one subquery form that never yields NULL: a row is either there or
// it is not, whatever that row happens to contain. That is also why its select
// list is unconstrained, where a scalar or IN subquery must produce exactly
// one column.
type ExistsSubquery struct {
	Input  Node
	Negate bool
}

// InSubquery is X IN (SELECT ...), or NOT IN when Negate is set.
type InSubquery struct {
	X      Expr
	Input  Node
	Negate bool
}

// Case is a CASE expression, in its searched form.
//
// The simple form, CASE x WHEN v THEN ..., is rewritten by the binder into the
// searched one by turning each arm into x = v. That follows the same reasoning
// as a comma in FROM becoming a cross join: two spellings of one thing produce
// one plan rather than a second code path through the executor.
//
// Else is nil when the clause was omitted, which SQL says yields NULL rather
// than an error -- a CASE that matches nothing is unknown, not wrong.
type Case struct {
	Whens []CaseWhen
	Else  Expr
	Kind  types.Kind
}

// CaseWhen is one arm. Cond is always a predicate, whichever form was written.
type CaseWhen struct {
	Cond, Value Expr
}

// InList is X IN (a, b, c), or NOT IN when Negate is set.
//
// It is a separate node from InSubquery rather than one node with two possible
// sources, because they are two constructs: one evaluates a fixed list of
// expressions over the current row, the other runs a plan.
type InList struct {
	X      Expr
	List   []Expr
	Negate bool
}

func (e *Column) Type() types.Kind { return e.Kind }
func (e *Const) Type() types.Kind  { return e.Val.Kind() }
func (e *Param) Type() types.Kind  { return e.Kind }
func (e *Binary) Type() types.Kind { return e.Kind }
func (e *Unary) Type() types.Kind  { return e.Kind }
func (e *IsNull) Type() types.Kind { return types.KindBool }
func (e *Cast) Type() types.Kind   { return e.Kind }

func (e *ScalarSubquery) Type() types.Kind { return e.Kind }
func (e *ExistsSubquery) Type() types.Kind { return types.KindBool }
func (e *InSubquery) Type() types.Kind     { return types.KindBool }
func (e *InList) Type() types.Kind         { return types.KindBool }
func (e *Case) Type() types.Kind           { return e.Kind }

func (*Cast) exprNode()   {}
func (*Column) exprNode() {}
func (*Const) exprNode()  {}
func (*Param) exprNode()  {}
func (*Binary) exprNode() {}
func (*Unary) exprNode()  {}
func (*IsNull) exprNode() {}

func (*ScalarSubquery) exprNode() {}
func (*ExistsSubquery) exprNode() {}
func (*InSubquery) exprNode()     {}
func (*InList) exprNode()         {}
func (*Case) exprNode()           {}

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

// Join combines two inputs. The output row is the left row followed by the
// right one, so a column's ordinal in the joined row is its ordinal within its
// own side plus the width of everything to the left of it — which is what the
// binder assigns, so nothing here has to translate ordinals per row.
//
// Pred is nil for a CROSS JOIN. For an outer join the side that may go
// unmatched is padded with NULLs rather than dropped.
type Join struct {
	Left, Right Node
	Type        ast.JoinType
	Pred        Expr
	Cols        []ResultColumn
}

// AggCall is one aggregate in an Aggregate node.
type AggCall struct {
	// Func names the aggregate; the executor resolves it through builtin.
	Func string
	// Arg is nil for count(*), which counts rows rather than values.
	Arg      Expr
	Distinct bool
	Kind     types.Kind
}

// Aggregate groups its input and folds each group down to one row.
//
// The output row is the group keys followed by the aggregate results, so an
// expression above it addresses either by ordinal like any other column. The
// binder rewrites the select list and HAVING against that layout, which is why
// nothing here needs to know what the original expressions looked like.
type Aggregate struct {
	Input Node
	// Keys are the GROUP BY expressions, evaluated against the input row.
	Keys []Expr
	Aggs []AggCall
	Cols []ResultColumn
}

// Distinct removes duplicate rows.
//
// On holds the expressions that decide uniqueness for DISTINCT ON; it is empty
// for a plain DISTINCT, which compares the whole output row. Those expressions
// need not appear in the select list, so the projection below may carry extra
// trailing columns that exist only to evaluate them. Width is how many leading
// columns to emit, which trims them off again without a second projection.
//
// The first row of each group wins, and the input order is preserved, so an
// ORDER BY below decides which row that is — the same rule PostgreSQL follows.
type Distinct struct {
	Input Node
	On    []Expr
	Width int
	Cols  []ResultColumn
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
func (n *Join) Result() []ResultColumn      { return n.Cols }
func (n *Aggregate) Result() []ResultColumn { return n.Cols }
func (n *Distinct) Result() []ResultColumn  { return n.Cols }
func (n *Project) Result() []ResultColumn   { return n.Cols }
func (n *Sort) Result() []ResultColumn      { return n.Input.Result() }
func (n *Limit) Result() []ResultColumn     { return n.Input.Result() }

func (*SingleRow) planNode() {}
func (*Scan) planNode()      {}
func (*Filter) planNode()    {}
func (*Join) planNode()      {}
func (*Aggregate) planNode() {}
func (*Distinct) planNode()  {}
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
