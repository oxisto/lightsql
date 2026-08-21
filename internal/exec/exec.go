package exec

import (
	"context"
	"errors"
	"math"
	"slices"

	"hash/maphash"

	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/builtin"
	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/storage"
	"github.com/oxisto/lightsql/internal/types"
)

// Operator produces rows one at a time.
//
// The iterator shape is what lets a LIMIT stop the scan beneath it and keeps an
// intermediate result from being materialised in full. The alternative — every
// operator returning a complete slice — makes short-circuiting impossible and
// allocates a fresh copy of the data at each level.
type Operator interface {
	// Next returns the next row, or ok false at end of input. The returned row
	// is only valid until the following call.
	Next(ctx context.Context) (row Row, ok bool, err error)
	// Close releases resources. It is safe to call more than once.
	Close() error
}

// Build compiles a plan node into an operator tree.
//
// The transaction is threaded through rather than read from a global, because a
// scan must read through the snapshot of the statement that asked for it: two
// statements running concurrently on pooled connections legitimately see
// different data.
//
// The context is taken here, and not only in Next, because building is not
// purely structural: LIMIT and OFFSET are evaluated now, and a subquery in
// expression position builds an operator tree of its own.
func Build(ctx context.Context, n plan.Node, tx *storage.Tx, args []types.Value) (Operator, error) {
	switch n := n.(type) {
	case *plan.Scan:
		return &scanOp{rows: n.Table.Rows(tx)}, nil

	case *plan.Filter:
		input, err := Build(ctx, n.Input, tx, args)
		if err != nil {
			return nil, err
		}
		pred, err := Compile(n.Pred, tx)
		if err != nil {
			return nil, err
		}
		return &filterOp{input: input, pred: pred, args: args}, nil

	case *plan.Join:
		left, err := Build(ctx, n.Left, tx, args)
		if err != nil {
			return nil, err
		}
		right, err := Build(ctx, n.Right, tx, args)
		if err != nil {
			return nil, err
		}
		op := &joinOp{
			left: left, right: right, args: args,
			leftWidth:  len(n.Left.Result()),
			rightWidth: len(n.Right.Result()),
			// Which side survives without a partner is the only thing that
			// separates the four outer flavours, so it is decided once here
			// rather than switched on per row.
			keepLeft:  n.Type == ast.LeftJoin || n.Type == ast.FullJoin,
			keepRight: n.Type == ast.RightJoin || n.Type == ast.FullJoin,
		}
		if n.Pred != nil {
			if op.pred, err = Compile(n.Pred, tx); err != nil {
				return nil, err
			}
		}
		return op, nil

	case *plan.Aggregate:
		input, err := Build(ctx, n.Input, tx, args)
		if err != nil {
			return nil, err
		}
		op := &aggregateOp{input: input, args: args, calls: n.Aggs}
		// Resolved once, here, rather than per group. A name the binder
		// accepted must exist, so a miss is a bug in the plan; reporting it is
		// the only safe answer, because carrying on with some other aggregate
		// would return a confidently wrong number.
		for _, c := range n.Aggs {
			if c.Arg == nil {
				op.funcs = append(op.funcs, nil)
				continue
			}
			agg, ok := builtin.LookupAggregate(c.Func)
			if !ok {
				return nil, pgerr.Newf(pgerr.InternalError,
					"unknown aggregate %q in plan", c.Func)
			}
			op.funcs = append(op.funcs, agg)
		}
		for _, k := range n.Keys {
			eval, err := Compile(k, tx)
			if err != nil {
				return nil, err
			}
			op.keys = append(op.keys, eval)
		}
		for _, c := range n.Aggs {
			if c.Arg == nil {
				op.argEvals = append(op.argEvals, nil)
				continue
			}
			eval, err := Compile(c.Arg, tx)
			if err != nil {
				return nil, err
			}
			op.argEvals = append(op.argEvals, eval)
		}
		return op, nil

	case *plan.Distinct:
		input, err := Build(ctx, n.Input, tx, args)
		if err != nil {
			return nil, err
		}
		op := &distinctOp{
			input: input, args: args, width: n.Width,
			seen: builtin.NewKeySet(),
		}
		for _, e := range n.On {
			eval, err := Compile(e, tx)
			if err != nil {
				return nil, err
			}
			op.on = append(op.on, eval)
		}
		return op, nil

	case *plan.SingleRow:
		return &singleRowOp{}, nil

	case *plan.Project:
		input, err := Build(ctx, n.Input, tx, args)
		if err != nil {
			return nil, err
		}
		evals := make([]Eval, len(n.Exprs))
		for i, e := range n.Exprs {
			if evals[i], err = Compile(e, tx); err != nil {
				return nil, err
			}
		}
		return &projectOp{input: input, evals: evals, args: args, out: make(Row, len(evals))}, nil

	case *plan.Sort:
		input, err := Build(ctx, n.Input, tx, args)
		if err != nil {
			return nil, err
		}
		keys := make([]sortKey, len(n.Keys))
		for i, k := range n.Keys {
			eval, err := Compile(k.Expr, tx)
			if err != nil {
				return nil, err
			}
			keys[i] = sortKey{eval: eval, desc: k.Desc, nullsFirst: k.NullsFirst}
		}
		return &sortOp{input: input, keys: keys, args: args}, nil

	case *plan.Limit:
		input, err := Build(ctx, n.Input, tx, args)
		if err != nil {
			return nil, err
		}
		count, err := rowCount(ctx, n.Count, tx, args, math.MaxInt64)
		if err != nil {
			return nil, err
		}
		offset, err := rowCount(ctx, n.Offset, tx, args, 0)
		if err != nil {
			return nil, err
		}
		return &limitOp{input: input, remaining: count, offset: offset}, nil

	default:
		return nil, pgerr.Newf(pgerr.InternalError, "cannot execute plan node %T", n)
	}
}

// rowCount evaluates a LIMIT or OFFSET operand, which cannot reference a column
// and so is constant for the whole statement.
func rowCount(ctx context.Context, e plan.Expr, tx *storage.Tx, args []types.Value, def int64) (int64, error) {
	if e == nil {
		return def, nil
	}
	eval, err := Compile(e, tx)
	if err != nil {
		return 0, err
	}
	v, err := eval(ctx, args, nil)
	if err != nil {
		return 0, err
	}
	if v.IsNull() {
		// PostgreSQL treats a NULL limit as no limit and a NULL offset as zero.
		return def, nil
	}
	if n := v.AsInt(); n >= 0 {
		return n, nil
	}
	return 0, pgerr.New(pgerr.InvalidTextForType, "LIMIT must not be negative")
}

// scanOp reads a table's rows.
type scanOp struct {
	rows [][]types.Value
	i    int
}

func (o *scanOp) Next(ctx context.Context) (Row, bool, error) {
	// Cancellation is checked per row rather than once per statement, so a long
	// scan actually stops when its context is cancelled.
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	row := o.rows[o.i]
	o.i++
	return row, true, nil
}

func (o *scanOp) Close() error { return nil }

// singleRowOp yields exactly one empty row, executing plan.SingleRow.
//
// The receiver must be a pointer: with a value receiver the assignment to done
// would be made on a copy, the operator would yield rows forever, and a query
// such as SELECT 1 would never terminate.
type singleRowOp struct{ done bool }

func (o *singleRowOp) Next(ctx context.Context) (Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	o.done = true
	return Row{}, true, nil
}

func (o *singleRowOp) Close() error { return nil }

// SliceOp yields rows that have already been computed, which is how a RETURNING
// clause is served: the statement has to finish modifying the table before its
// output can be read, so those rows are materialised rather than streamed.
type SliceOp struct {
	rows []Row
	i    int
}

// NewSliceOp returns an operator over an existing slice of rows.
func NewSliceOp(rows []Row) *SliceOp { return &SliceOp{rows: rows} }

func (o *SliceOp) Next(ctx context.Context) (Row, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	row := o.rows[o.i]
	o.i++
	return row, true, nil
}

func (o *SliceOp) Close() error { return nil }

// filterOp drops rows whose predicate is not true. A predicate that evaluates to
// unknown drops the row just as false does; that is SQL's rule and the reason
// comparisons return three-valued results.
type filterOp struct {
	input Operator
	pred  Eval
	args  []types.Value
}

func (o *filterOp) Next(ctx context.Context) (Row, bool, error) {
	for {
		row, ok, err := o.input.Next(ctx)
		if err != nil || !ok {
			return nil, false, err
		}
		v, err := o.pred(ctx, o.args, row)
		if err != nil {
			return nil, false, err
		}
		if v.Truth().IsTrue() {
			return row, true, nil
		}
	}
}

func (o *filterOp) Close() error { return o.input.Close() }

// projectOp evaluates the select list over each input row.
type projectOp struct {
	input Operator
	evals []Eval
	args  []types.Value
	// out is reused across rows; the contract is that a row is only valid until
	// the next call to Next, and the driver copies before returning.
	out Row
}

func (o *projectOp) Next(ctx context.Context) (Row, bool, error) {
	row, ok, err := o.input.Next(ctx)
	if err != nil || !ok {
		return nil, false, err
	}
	for i, eval := range o.evals {
		if o.out[i], err = eval(ctx, o.args, row); err != nil {
			return nil, false, err
		}
	}
	return o.out, true, nil
}

func (o *projectOp) Close() error { return o.input.Close() }

// sortKey is one compiled ORDER BY term.
type sortKey struct {
	eval       Eval
	desc       bool
	nullsFirst bool
}

// sortOp orders its input.
//
// Sorting cannot stream: the last row read may belong first, so the whole input
// is drained before anything is emitted. That makes this the one operator whose
// memory is proportional to the result, which is acceptable at the scale
// lightsql targets and is why the ordering rules live here rather than being
// pushed into the scan.
type sortOp struct {
	input  Operator
	keys   []sortKey
	args   []types.Value
	rows   []Row
	i      int
	sorted bool
}

func (o *sortOp) Next(ctx context.Context) (Row, bool, error) {
	if !o.sorted {
		if err := o.drainAndSort(ctx); err != nil {
			return nil, false, err
		}
	}
	if o.i >= len(o.rows) {
		return nil, false, nil
	}
	row := o.rows[o.i]
	o.i++
	return row, true, nil
}

func (o *sortOp) drainAndSort(ctx context.Context) error {
	// Sort keys are computed once per row here rather than inside the
	// comparison, which would recompute them O(n log n) times.
	type entry struct {
		row  Row
		keys []types.Value
	}
	var entries []entry

	for {
		row, ok, err := o.input.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}

		keys := make([]types.Value, len(o.keys))
		for i, k := range o.keys {
			if keys[i], err = k.eval(ctx, o.args, row); err != nil {
				return err
			}
		}
		// The operator contract says a row is only valid until the next Next,
		// so any operator that accumulates rows must copy them. Sorting is the
		// first place that matters.
		owned := make(Row, len(row))
		copy(owned, row)
		entries = append(entries, entry{row: owned, keys: keys})
	}

	// The comparison cannot fail, because every key was already evaluated above.
	// That is the reason for precomputing them beyond the wasted work: a sort
	// comparator has nowhere to report an error to.
	//
	// A stable sort makes the result deterministic for rows that tie on every
	// key. SQL does not require that, but a test asserting on output should not
	// depend on which of two equal rows came back first.
	slices.SortStableFunc(entries, func(a, b entry) int {
		for i, k := range o.keys {
			if c := compareSortKey(a.keys[i], b.keys[i], k.desc, k.nullsFirst); c != 0 {
				return c
			}
		}
		return 0
	})

	o.rows = make([]Row, len(entries))
	for i, e := range entries {
		o.rows[i] = e.row
	}
	o.sorted = true
	return nil
}

// compareSortKey orders two key values under one ORDER BY term.
//
// NULL placement is deliberately independent of direction: DESC reverses the
// ordering of the values, but where NULLs land is decided by nullsFirst, which
// the binder already resolved from the term's direction and any explicit NULLS
// FIRST/LAST. Folding the two together is how DESC NULLS LAST ends up putting
// NULLs first.
func compareSortKey(a, b types.Value, desc, nullsFirst bool) int {
	aNull, bNull := a.IsNull(), b.IsNull()
	switch {
	case aNull && bNull:
		return 0
	case aNull:
		if nullsFirst {
			return -1
		}
		return 1
	case bNull:
		if nullsFirst {
			return 1
		}
		return -1
	}

	c := types.Compare(a, b)
	if desc {
		return -c
	}
	return c
}

func (o *sortOp) Close() error { return o.input.Close() }

// limitOp applies OFFSET and LIMIT.
type limitOp struct {
	input     Operator
	remaining int64
	offset    int64
}

func (o *limitOp) Next(ctx context.Context) (Row, bool, error) {
	for o.offset > 0 {
		_, ok, err := o.input.Next(ctx)
		if err != nil || !ok {
			return nil, false, err
		}
		o.offset--
	}
	if o.remaining <= 0 {
		return nil, false, nil
	}
	row, ok, err := o.input.Next(ctx)
	if err != nil || !ok {
		return nil, false, err
	}
	o.remaining--
	return row, true, nil
}

func (o *limitOp) Close() error { return o.input.Close() }

// Result is what a data-modifying statement produced: a count, and the rows a
// RETURNING clause asked for.
type Result struct {
	Affected int64
	// Rows is nil unless the statement had a RETURNING clause.
	Rows []Row
}

// compiledCheck is a CHECK constraint ready to evaluate against a row.
type compiledCheck struct {
	name string
	pred Eval
}

func compileChecks(checks []plan.Check, tx *storage.Tx) ([]compiledCheck, error) {
	if len(checks) == 0 {
		return nil, nil
	}
	out := make([]compiledCheck, len(checks))
	for i, c := range checks {
		pred, err := Compile(c.Pred, tx)
		if err != nil {
			return nil, err
		}
		out[i] = compiledCheck{name: c.Name, pred: pred}
	}
	return out, nil
}

// runChecks evaluates every CHECK against a row.
//
// A check is satisfied when its predicate is true *or* unknown, and violated
// only by false. That is deliberately the opposite of a WHERE clause, which
// keeps only true — and it is why `CHECK (n >= 0)` does not reject a row whose
// n is NULL. Reusing the filter rule here would silently make every CHECK an
// implicit NOT NULL.
func runChecks(ctx context.Context, checks []compiledCheck, args []types.Value, row Row, table string) error {
	for _, c := range checks {
		v, err := c.pred(ctx, args, row)
		if err != nil {
			return err
		}
		if v.Truth() == types.False {
			return pgerr.Newf(pgerr.CheckViolation,
				"new row for relation %q violates check constraint %q", table, c.name)
		}
	}
	return nil
}

// returningEval compiles a RETURNING list once, ahead of the row loop.
type returningEval struct {
	evals []Eval
}

func compileReturning(r *plan.Returning, tx *storage.Tx) (*returningEval, error) {
	if r == nil {
		return nil, nil
	}
	evals := make([]Eval, len(r.Exprs))
	for i, e := range r.Exprs {
		var err error
		if evals[i], err = Compile(e, tx); err != nil {
			return nil, err
		}
	}
	return &returningEval{evals: evals}, nil
}

// row evaluates the RETURNING list against one affected row. A fresh slice is
// allocated per row because, unlike an operator's output, these are accumulated
// rather than consumed immediately.
func (r *returningEval) row(ctx context.Context, args []types.Value, in Row) (Row, error) {
	out := make(Row, len(r.evals))
	for i, eval := range r.evals {
		var err error
		if out[i], err = eval(ctx, args, in); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ExecUpdate applies an UPDATE.
func ExecUpdate(ctx context.Context, tx *storage.Tx, up *plan.Update, args []types.Value) (Result, error) {
	pred, err := compilePredicate(up.Where, tx)
	if err != nil {
		return Result{}, err
	}
	ret, err := compileReturning(up.Returning, tx)
	if err != nil {
		return Result{}, err
	}
	checks, err := compileChecks(up.Checks, tx)
	if err != nil {
		return Result{}, err
	}
	assign := make([]Eval, len(up.Assignments))
	for i, a := range up.Assignments {
		if assign[i], err = Compile(a.Value, tx); err != nil {
			return Result{}, err
		}
	}

	var res Result
	wrote := touched{}
	for _, v := range up.Table.Scan(tx) {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		match, err := pred(ctx, args, v.Vals)
		if err != nil {
			return Result{}, err
		}
		if !match {
			continue
		}

		// The replacement is a fresh slice, and every assignment evaluates
		// against the original row: that is what makes `SET a = b, b = a` a
		// swap rather than two copies of b.
		next := make([]types.Value, len(v.Vals))
		copy(next, v.Vals)
		for i, a := range up.Assignments {
			val, err := assign[i](ctx, args, v.Vals)
			if err != nil {
				return Result{}, err
			}
			next[a.Ordinal] = val
		}

		if err := runChecks(ctx, checks, args, next, up.Table.Name); err != nil {
			return Result{}, err
		}
		// A key the children point at is changing, so they have to be dealt
		// with before the parent row moves out from under them.
		if err := applyRefActions(tx, up.Table, v.Vals, next, onUpdate, wrote); err != nil {
			return Result{}, err
		}
		if err := up.Table.Update(tx, v, next); err != nil {
			return Result{}, err
		}

		res.Affected++
		if ret != nil {
			// RETURNING on an UPDATE reports the new values.
			out, err := ret.row(ctx, args, next)
			if err != nil {
				return Result{}, err
			}
			res.Rows = append(res.Rows, out)
		}
	}

	// Uniqueness is checked once the statement has finished writing, so a row
	// keeping its own value does not conflict with itself.
	if err := up.Table.CheckConstraints(tx); err != nil {
		return Result{}, err
	}
	if err := up.Table.CheckForeignKeys(tx); err != nil {
		return Result{}, err
	}
	if err := wrote.check(tx, up.Table); err != nil {
		return Result{}, err
	}
	return res, nil
}

// ExecDelete applies a DELETE.
func ExecDelete(ctx context.Context, tx *storage.Tx, del *plan.Delete, args []types.Value) (Result, error) {
	pred, err := compilePredicate(del.Where, tx)
	if err != nil {
		return Result{}, err
	}
	ret, err := compileReturning(del.Returning, tx)
	if err != nil {
		return Result{}, err
	}

	var res Result
	wrote := touched{}
	for _, v := range del.Table.Scan(tx) {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		match, err := pred(ctx, args, v.Vals)
		if err != nil {
			return Result{}, err
		}
		if !match {
			continue
		}
		if ret != nil {
			// RETURNING on a DELETE reports the row as it was before removal.
			out, err := ret.row(ctx, args, v.Vals)
			if err != nil {
				return Result{}, err
			}
			res.Rows = append(res.Rows, out)
		}
		if err := applyRefActions(tx, del.Table, v.Vals, nil, onDelete, wrote); err != nil {
			return Result{}, err
		}
		if err := del.Table.Delete(tx, v); err != nil {
			return Result{}, err
		}
		res.Affected++
	}

	// A DELETE had no statement-end check at all, so a cascade could leave a
	// child in a state nothing revalidated.
	if err := wrote.check(tx, del.Table); err != nil {
		return Result{}, err
	}
	return res, nil
}

// touched collects the tables a statement wrote to through referential actions.
//
// Their constraints cannot be checked as each row is written, for the same
// reason the directly targeted table's cannot: a set of changes is only
// consistent once all of it has been applied.
type touched map[*catalog.Table]struct{}

func (t touched) add(tbl *catalog.Table) { t[tbl] = struct{}{} }

// check revalidates every table a cascade wrote to, skipping the statement's
// own target, which the caller checks separately.
func (t touched) check(tx *storage.Tx, except *catalog.Table) error {
	for tbl := range t {
		if tbl == except {
			continue
		}
		if err := tbl.CheckConstraints(tx); err != nil {
			return err
		}
		if err := tbl.CheckForeignKeys(tx); err != nil {
			return err
		}
	}
	return nil
}

// refEvent distinguishes which of a foreign key's two actions applies.
type refEvent int

const (
	onDelete refEvent = iota
	onUpdate
)

// applyRefActions deals with the rows referencing a parent row that is about to
// be deleted or have its key changed.
//
// next is the replacement row for an update, or nil for a delete. An update
// whose key columns are unchanged is not a referential event at all, which is
// why the comparison happens here rather than at the call site: `UPDATE parent
// SET name = ...` must not cascade.
func applyRefActions(tx *storage.Tx, parent *catalog.Table, old, next []types.Value,
	ev refEvent, wrote touched) error {

	if !parent.IsReferenced() {
		return nil
	}

	for _, group := range parent.ChildrenOf(tx, old) {
		fk := group.Ref.FK
		action := fk.OnDelete
		if ev == onUpdate {
			action = fk.OnUpdate
			// Only a change to the referenced columns matters.
			if next != nil && matchesParentKey(old, next, fk.ParentCols) {
				continue
			}
		}

		for _, child := range group.Rows {
			if err := applyRefAction(tx, group.Ref.Child, fk, child, next, action); err != nil {
				return err
			}
			// A cascade writes to the child, so the child's own constraints
			// have to be revalidated once the statement finishes. Without this
			// SET DEFAULT can leave a row that both violates a CHECK and points
			// at no parent — the foreign-key machinery breaking a foreign key.
			wrote.add(group.Ref.Child)
		}
	}
	return nil
}

func matchesParentKey(old, next []types.Value, cols []int) bool {
	for _, ord := range cols {
		if !types.Equal(old[ord], next[ord]) {
			return false
		}
	}
	return true
}

func applyRefAction(tx *storage.Tx, childTable *catalog.Table, fk *catalog.ForeignKey,
	child *storage.Version, next []types.Value, action catalog.RefAction) error {

	switch action {
	case catalog.Cascade:
		if next == nil {
			return childTable.Delete(tx, child)
		}
		// The parent's key moved, so the child follows it.
		updated := make([]types.Value, len(child.Vals))
		copy(updated, child.Vals)
		for i, ord := range fk.Columns {
			updated[ord] = next[fk.ParentCols[i]]
		}
		return childTable.Update(tx, child, updated)

	case catalog.SetNull, catalog.SetDefault:
		updated := make([]types.Value, len(child.Vals))
		copy(updated, child.Vals)
		for _, ord := range fk.Columns {
			v, err := refReplacement(childTable, ord, action)
			if err != nil {
				return err
			}
			updated[ord] = v
		}
		return childTable.Update(tx, child, updated)

	default: // NoAction, Restrict
		return pgerr.Newf(pgerr.ForeignKeyViolation,
			"update or delete on table %q violates foreign key constraint %q on table %q",
			fk.Parent.Name, fk.Name, childTable.Name).
			WithDetail("Key is still referenced from table %q.", childTable.Name)
	}
}

// refReplacement produces the value SET NULL or SET DEFAULT puts in a
// referencing column.
func refReplacement(t *catalog.Table, ordinal int, action catalog.RefAction) (types.Value, error) {
	if action == catalog.SetNull {
		return types.Null(), nil
	}
	// SET DEFAULT without a DEFAULT on the column means NULL, which then has to
	// satisfy NOT NULL like any other value rather than being waved through.
	col := t.Columns[ordinal]
	if col.Default == nil {
		return types.Null(), nil
	}
	return t.EvalDefault(ordinal)
}

// compilePredicate turns an optional WHERE into a row test. A missing clause
// matches every row, and a clause evaluating to unknown matches none — the same
// rule a Filter operator applies.
func compilePredicate(e plan.Expr, tx *storage.Tx) (func(ctx context.Context, args []types.Value, row Row) (bool, error), error) {
	if e == nil {
		return func(context.Context, []types.Value, Row) (bool, error) { return true, nil }, nil
	}
	eval, err := Compile(e, tx)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, args []types.Value, row Row) (bool, error) {
		v, err := eval(ctx, args, row)
		if err != nil {
			return false, err
		}
		return v.Truth().IsTrue(), nil
	}, nil
}

// ExecInsert runs an INSERT.
func ExecInsert(ctx context.Context, tx *storage.Tx, ins *plan.Insert, args []types.Value) (Result, error) {
	width := len(ins.Table.Columns)
	ret, err := compileReturning(ins.Returning, tx)
	if err != nil {
		return Result{}, err
	}
	checks, err := compileChecks(ins.Checks, tx)
	if err != nil {
		return Result{}, err
	}
	defaults := make(map[int]Eval, len(ins.Defaults))
	for ord, e := range ins.Defaults {
		if defaults[ord], err = Compile(e, tx); err != nil {
			return Result{}, err
		}
	}

	conflict, err := compileConflict(ins, tx)
	if err != nil {
		return Result{}, err
	}

	var res Result

	// store completes and writes one row from the values for the target
	// columns. Both spellings of INSERT go through it, so a serial, a default,
	// a CHECK and a RETURNING clause cannot come to mean different things
	// depending on whether the rows came from VALUES or from a SELECT.
	store := func(vals Row) error {
		// Columns the statement did not name start as NULL, then serials are
		// filled from their sequence.
		row := make(Row, width)
		for i := range row {
			row[i] = types.Null()
		}
		for i, v := range vals {
			row[ins.Targets[i]] = v
		}
		for _, ord := range ins.Serials {
			row[ord] = types.Int(ins.Table.NextSerial(ord))
		}
		// A DEFAULT is evaluated once per row rather than hoisted out of the
		// loop. Every default is a constant expression today, so this makes no
		// difference yet — but a sequence-backed default such as nextval must
		// yield a distinct value per row, and hoisting would quietly give a
		// multi-row INSERT the same one throughout.
		for ord, eval := range defaults {
			v, err := eval(ctx, args, nil)
			if err != nil {
				return err
			}
			row[ord] = v
		}

		// ON CONFLICT is resolved before the row is written, not after a
		// violation is raised: the constraint check runs at the end of the
		// statement over the whole table, by which point it is too late to know
		// which row collided or to do anything but fail.
		if conflict != nil {
			if existing := conflict.find(tx, ins.Table, row); existing != nil {
				return conflict.resolve(ctx, tx, ins.Table, existing, row, args, checks, ret, &res)
			}
		}

		if err := runChecks(ctx, checks, args, row, ins.Table.Name); err != nil {
			return err
		}
		if err := ins.Table.Insert(tx, row); err != nil {
			return err
		}
		res.Affected++

		// RETURNING is evaluated after the row is complete, so a generated
		// serial is visible to it.
		if ret != nil {
			out, err := ret.row(ctx, args, row)
			if err != nil {
				return err
			}
			res.Rows = append(res.Rows, out)
		}
		return nil
	}

	if ins.Source != nil {
		if err := insertFrom(ctx, ins.Source, tx, args, store); err != nil {
			return res, err
		}
	}

	vals := make(Row, len(ins.Targets))
	for _, exprs := range ins.Rows {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		for i, e := range exprs {
			eval, err := Compile(e, tx)
			if err != nil {
				return res, err
			}
			if vals[i], err = eval(ctx, args, nil); err != nil {
				return res, err
			}
		}
		if err := store(vals); err != nil {
			return res, err
		}
	}

	// Checked once the statement has written every row, so a multi-row INSERT
	// is validated against its own rows as well as the existing ones.
	if err := ins.Table.CheckConstraints(tx); err != nil {
		return res, err
	}
	if err := ins.Table.CheckForeignKeys(tx); err != nil {
		return res, err
	}
	return res, nil
}

// insertFrom drives an INSERT ... SELECT, handing each source row to store.
//
// The rows are consumed as they are produced rather than collected first, so a
// large SELECT does not have to be materialised. That is safe even when the
// source reads the table being written, because a scan takes its rows when the
// operator is built: the statement sees the table as it was, and cannot chase
// the rows it is itself appending.
func insertFrom(ctx context.Context, src plan.Node, tx *storage.Tx, args []types.Value, store func(Row) error) (err error) {
	op, err := Build(ctx, src, tx, args)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, op.Close()) }()

	for {
		row, ok, err := op.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := store(row); err != nil {
			return err
		}
	}
}

// ExecRenameTable runs ALTER TABLE ... RENAME TO.
func ExecRenameTable(cat *catalog.Catalog, rt *plan.RenameTable) error {
	return cat.RenameTable(rt.Schema, rt.From, rt.To)
}

// ExecRenameColumn runs ALTER TABLE ... RENAME COLUMN.
func ExecRenameColumn(cat *catalog.Catalog, rc *plan.RenameColumn) error {
	return cat.RenameColumn(rc.Schema, rc.Table, rc.From, rc.To)
}

// ExecCreateIndex runs a CREATE INDEX.
func ExecCreateIndex(cat *catalog.Catalog, ci *plan.CreateIndex) error {
	_, err := cat.CreateIndex(ci.Table.Schema, ci.Table.Name, ci.Index, ci.IfNotExists)
	return err
}

// ExecDropIndex runs a DROP INDEX.
func ExecDropIndex(cat *catalog.Catalog, di *plan.DropIndex) error {
	for _, n := range di.Names {
		if err := cat.DropIndex(di.Schema, n, di.IfExists); err != nil {
			return err
		}
	}
	return nil
}

// ExecDropTable runs a DROP TABLE.
func ExecDropTable(cat *catalog.Catalog, dt *plan.DropTable) error {
	return cat.DropTable(dt.Names, dt.IfExists)
}

// ExecCreateTable runs a CREATE TABLE.
func ExecCreateTable(cat *catalog.Catalog, ct *plan.CreateTable) error {
	_, err := cat.CreateTable(ct.Table, ct.IfNotExists)
	return err
}

// joinOp joins two inputs with a nested loop.
//
// The left side streams; the right side is read once into memory and rescanned
// for each left row. An operator cannot be rewound, so the right side has to be
// materialised somewhere, and doing it here keeps the cost to one copy of the
// smaller relation rather than one per level of the plan. At the scale lightsql
// targets that is the right trade; a hash build over the right side is the next
// step if a join ever shows up in a profile.
//
// All five flavours share this one loop. An outer join differs only in whether
// an unmatched row is emitted padded with NULLs, which is two booleans decided
// at build time.
type joinOp struct {
	left, right Operator
	pred        Eval
	args        []types.Value

	leftWidth, rightWidth int
	keepLeft, keepRight   bool

	// rightRows is the materialised right side, filled on the first Next.
	rightRows []Row
	built     bool
	// matched marks, per right row, whether it ever found a partner. Only a
	// RIGHT or FULL join needs it, but tracking it unconditionally costs one
	// bool per row and removes a branch from the inner loop.
	matched []bool

	cur      Row  // current left row
	haveCur  bool // whether cur holds a row
	curFound bool // whether cur has matched anything yet
	j        int  // position in rightRows for the current left row

	// drain is the index into rightRows once the left side is exhausted and
	// unmatched right rows are being emitted.
	drain    int
	draining bool

	out Row
}

func (o *joinOp) build(ctx context.Context) error {
	for {
		row, ok, err := o.right.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		// Next promises its row only until the following call, so a row that
		// outlives that has to be copied.
		cp := make(Row, len(row))
		copy(cp, row)
		o.rightRows = append(o.rightRows, cp)
	}
	o.matched = make([]bool, len(o.rightRows))
	o.built = true
	return nil
}

func (o *joinOp) Next(ctx context.Context) (Row, bool, error) {
	if !o.built {
		if err := o.build(ctx); err != nil {
			return nil, false, err
		}
	}
	if o.out == nil {
		o.out = make(Row, o.leftWidth+o.rightWidth)
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}

		if o.draining {
			for o.drain < len(o.rightRows) {
				i := o.drain
				o.drain++
				if !o.matched[i] {
					return o.emit(nil, o.rightRows[i]), true, nil
				}
			}
			return nil, false, nil
		}

		if !o.haveCur {
			row, ok, err := o.left.Next(ctx)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				// The left side is done. A RIGHT or FULL join still owes the
				// right rows that never matched.
				o.draining = o.keepRight
				if !o.draining {
					return nil, false, nil
				}
				continue
			}
			o.cur = row
			o.haveCur, o.curFound, o.j = true, false, 0
		}

		if o.j < len(o.rightRows) {
			i := o.j
			o.j++
			ok, err := o.match(ctx, o.cur, o.rightRows[i])
			if err != nil {
				return nil, false, err
			}
			if ok {
				o.curFound = true
				o.matched[i] = true
				// match already built the joined row to evaluate the
				// predicate against, and a matched pair fills it completely,
				// so there is nothing left for emit to do.
				return o.out, true, nil
			}
			continue
		}

		// The current left row is exhausted against the right side.
		o.haveCur = false
		if o.keepLeft && !o.curFound {
			return o.emit(o.cur, nil), true, nil
		}
	}
}

// match builds the joined row and evaluates the join condition over it.
//
// The row is built whether or not there is a condition, so that a caller which
// gets true can return o.out as it stands. A CROSS JOIN has no condition, so
// every pair matches. Only true joins the rows: false and unknown are both
// rejected, which is the same rule a WHERE clause follows.
func (o *joinOp) match(ctx context.Context, l, r Row) (bool, error) {
	copy(o.out, l)
	copy(o.out[o.leftWidth:], r)
	if o.pred == nil {
		return true, nil
	}
	v, err := o.pred(ctx, o.args, o.out)
	if err != nil {
		return false, err
	}
	return v.Truth() == types.True, nil
}

// emit builds an output row for an unmatched left or right row. The absent
// side is padded with NULLs, which is what makes an outer join's missing half
// readable as SQL NULL rather than as a zero value. A matched pair does not
// come through here, since match has already built that row.
func (o *joinOp) emit(l, r Row) Row {
	for i := range o.out {
		o.out[i] = types.Null()
	}
	if l != nil {
		copy(o.out, l)
	}
	if r != nil {
		copy(o.out[o.leftWidth:], r)
	}
	return o.out
}

func (o *joinOp) Close() error {
	err := o.left.Close()
	if rerr := o.right.Close(); err == nil {
		err = rerr
	}
	return err
}

// aggregateOp groups its input and folds each group to one row.
//
// Groups are found by hashing the key values, and a bucket keeps its rows in
// arrival order so the output does not depend on Go's map iteration. SQL does
// not promise an order for GROUP BY, but a result that reshuffles between runs
// makes a test suite flaky for no reason, and input order is the one order that
// costs nothing to preserve.
type aggregateOp struct {
	input    Operator
	args     []types.Value
	keys     []Eval
	calls    []plan.AggCall
	argEvals []Eval

	// funcs mirrors calls, resolved at build time. A nil entry is count(*),
	// which has no argument to fold.
	funcs  []*builtin.Aggregate
	groups []*aggGroup
	// index maps a key hash to the groups carrying it. Collisions are resolved
	// by comparing the key values, so two different keys that happen to hash
	// alike stay separate.
	index map[uint64][]*aggGroup
	seed  maphash.Seed

	built bool
	i     int
	out   Row
}

type aggGroup struct {
	key  []types.Value
	accs []builtin.Accumulator
}

func (o *aggregateOp) newGroup(key []types.Value) *aggGroup {
	g := &aggGroup{key: key, accs: make([]builtin.Accumulator, len(o.calls))}
	for i, c := range o.calls {
		if o.funcs[i] == nil {
			g.accs[i] = builtin.CountStar()
			continue
		}
		acc := o.funcs[i].New()
		if c.Distinct {
			acc = builtin.Distinct(acc)
		}
		g.accs[i] = acc
	}
	return g
}

func (o *aggregateOp) hash(key []types.Value) uint64 {
	var h maphash.Hash
	h.SetSeed(o.seed)
	for _, v := range key {
		v.Hash(&h)
	}
	return h.Sum64()
}

// find returns the group for a key, creating it on first sight.
func (o *aggregateOp) find(key []types.Value) *aggGroup {
	sum := o.hash(key)
	for _, g := range o.index[sum] {
		if sameKey(g.key, key) {
			return g
		}
	}
	g := o.newGroup(key)
	o.index[sum] = append(o.index[sum], g)
	o.groups = append(o.groups, g)
	return g
}

// sameKey compares two group keys.
//
// It uses Compare, the total order, rather than Eq: grouping must put NULLs
// together, and Eq would answer unknown for a pair of them, so every NULL key
// would become its own group. PostgreSQL groups NULLs into one group, and this
// is where that difference lives.
func sameKey(a, b []types.Value) bool {
	for i := range a {
		if types.Compare(a[i], b[i]) != 0 {
			return false
		}
	}
	return true
}

func (o *aggregateOp) build(ctx context.Context) error {
	o.index = make(map[uint64][]*aggGroup)
	o.seed = maphash.MakeSeed()

	for {
		row, ok, err := o.input.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}

		key := make([]types.Value, len(o.keys))
		for i, k := range o.keys {
			if key[i], err = k(ctx, o.args, row); err != nil {
				return err
			}
		}
		g := o.find(key)

		for i, eval := range o.argEvals {
			if eval == nil {
				// count(*) counts the row, so there is nothing to evaluate.
				g.accs[i].Add(types.Null())
				continue
			}
			v, err := eval(ctx, o.args, row)
			if err != nil {
				return err
			}
			g.accs[i].Add(v)
		}
	}

	// An aggregate with no GROUP BY reports one row even over no input:
	// SELECT count(*) FROM empty is 0, not no rows at all. With a GROUP BY
	// there are no groups, so there are no rows, which is the other half of
	// the same rule.
	if len(o.groups) == 0 && len(o.keys) == 0 {
		o.groups = append(o.groups, o.newGroup(nil))
	}

	o.built = true
	return nil
}

func (o *aggregateOp) Next(ctx context.Context) (Row, bool, error) {
	if !o.built {
		if err := o.build(ctx); err != nil {
			return nil, false, err
		}
		o.out = make(Row, len(o.keys)+len(o.calls))
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if o.i >= len(o.groups) {
		return nil, false, nil
	}

	g := o.groups[o.i]
	o.i++
	copy(o.out, g.key)
	for i, acc := range g.accs {
		o.out[len(o.keys)+i] = acc.Result()
	}
	return o.out, true, nil
}

func (o *aggregateOp) Close() error { return o.input.Close() }

// distinctOp drops rows whose key has been seen before.
//
// It streams: a row is emitted as soon as it is known to be new, rather than
// the whole input being collected and deduplicated at the end, so a LIMIT above
// it can stop early. An ORDER BY below still has to materialise, since sorting
// cannot answer before it has seen everything; the streaming matters for the
// unordered case and for keeping one copy of the keys rather than two of the
// rows.
//
// The first row of each key wins, so the input order decides which one that is
// and the output preserves it. Both are what DISTINCT ON relies on.
type distinctOp struct {
	input Operator
	args  []types.Value
	// on is empty for a plain DISTINCT, which keys on the whole output row.
	on    []Eval
	width int
	seen  *builtin.KeySet
	key   []types.Value
}

func (o *distinctOp) Next(ctx context.Context) (Row, bool, error) {
	for {
		row, ok, err := o.input.Next(ctx)
		if err != nil || !ok {
			return nil, false, err
		}
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}

		if len(o.on) == 0 {
			// A plain DISTINCT compares the row the query actually returns,
			// not anything the projection appended.
			if !o.seen.Add(row[:o.width]) {
				continue
			}
			return row[:o.width], true, nil
		}

		if o.key == nil {
			o.key = make([]types.Value, len(o.on))
		}
		for i, eval := range o.on {
			if o.key[i], err = eval(ctx, o.args, row); err != nil {
				return nil, false, err
			}
		}
		if !o.seen.Add(o.key) {
			continue
		}
		return row[:o.width], true, nil
	}
}

func (o *distinctOp) Close() error { return o.input.Close() }
