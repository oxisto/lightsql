package exec

import (
	"context"
	"math"
	"slices"

	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
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
func Build(n plan.Node, args []types.Value) (Operator, error) {
	switch n := n.(type) {
	case *plan.Scan:
		return &scanOp{rows: n.Table.Rows()}, nil

	case *plan.Filter:
		input, err := Build(n.Input, args)
		if err != nil {
			return nil, err
		}
		pred, err := Compile(n.Pred)
		if err != nil {
			return nil, err
		}
		return &filterOp{input: input, pred: pred, args: args}, nil

	case *plan.Project:
		var input Operator = &emptyRowOp{}
		if n.Input != nil {
			var err error
			if input, err = Build(n.Input, args); err != nil {
				return nil, err
			}
		}
		evals := make([]Eval, len(n.Exprs))
		for i, e := range n.Exprs {
			var err error
			if evals[i], err = Compile(e); err != nil {
				return nil, err
			}
		}
		return &projectOp{input: input, evals: evals, args: args, out: make(Row, len(evals))}, nil

	case *plan.Sort:
		input, err := Build(n.Input, args)
		if err != nil {
			return nil, err
		}
		keys := make([]sortKey, len(n.Keys))
		for i, k := range n.Keys {
			eval, err := Compile(k.Expr)
			if err != nil {
				return nil, err
			}
			keys[i] = sortKey{eval: eval, desc: k.Desc, nullsFirst: k.NullsFirst}
		}
		return &sortOp{input: input, keys: keys, args: args}, nil

	case *plan.Limit:
		input, err := Build(n.Input, args)
		if err != nil {
			return nil, err
		}
		count, err := rowCount(n.Count, args, math.MaxInt64)
		if err != nil {
			return nil, err
		}
		offset, err := rowCount(n.Offset, args, 0)
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
func rowCount(e plan.Expr, args []types.Value, def int64) (int64, error) {
	if e == nil {
		return def, nil
	}
	eval, err := Compile(e)
	if err != nil {
		return 0, err
	}
	v, err := eval(args, nil)
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

// emptyRowOp yields exactly one empty row, which is what a SELECT without FROM
// projects over.
//
// The receiver must be a pointer: with a value receiver the assignment to done
// would be made on a copy, the operator would yield rows forever, and a query
// such as SELECT 1 would never terminate.
type emptyRowOp struct{ done bool }

func (o *emptyRowOp) Next(ctx context.Context) (Row, bool, error) {
	if o.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	o.done = true
	return Row{}, true, nil
}

func (o *emptyRowOp) Close() error { return nil }

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
		v, err := o.pred(o.args, row)
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
		if o.out[i], err = eval(o.args, row); err != nil {
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
			if keys[i], err = k.eval(o.args, row); err != nil {
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

// returningEval compiles a RETURNING list once, ahead of the row loop.
type returningEval struct {
	evals []Eval
}

func compileReturning(r *plan.Returning) (*returningEval, error) {
	if r == nil {
		return nil, nil
	}
	evals := make([]Eval, len(r.Exprs))
	for i, e := range r.Exprs {
		var err error
		if evals[i], err = Compile(e); err != nil {
			return nil, err
		}
	}
	return &returningEval{evals: evals}, nil
}

// row evaluates the RETURNING list against one affected row. A fresh slice is
// allocated per row because, unlike an operator's output, these are accumulated
// rather than consumed immediately.
func (r *returningEval) row(args []types.Value, in Row) (Row, error) {
	out := make(Row, len(r.evals))
	for i, eval := range r.evals {
		var err error
		if out[i], err = eval(args, in); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ExecUpdate applies an UPDATE.
func ExecUpdate(ctx context.Context, up *plan.Update, args []types.Value) (Result, error) {
	pred, err := compilePredicate(up.Where)
	if err != nil {
		return Result{}, err
	}
	ret, err := compileReturning(up.Returning)
	if err != nil {
		return Result{}, err
	}
	assign := make([]Eval, len(up.Assignments))
	for i, a := range up.Assignments {
		if assign[i], err = Compile(a.Value); err != nil {
			return Result{}, err
		}
	}

	var res Result
	err = up.Table.Mutate(func(row []types.Value) ([]types.Value, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match, err := pred(args, row)
		if err != nil {
			return nil, err
		}
		if !match {
			return row, nil
		}

		// The replacement is a copy, so a concurrent reader holding the old row
		// never observes a partially applied update. Every assignment also
		// evaluates against the original row, which is what makes
		// `SET a = b, b = a` a swap rather than two copies of b.
		next := make([]types.Value, len(row))
		copy(next, row)
		for i, a := range up.Assignments {
			v, err := assign[i](args, row)
			if err != nil {
				return nil, err
			}
			next[a.Ordinal] = v
		}

		res.Affected++
		if ret != nil {
			// RETURNING on an UPDATE reports the new values.
			out, err := ret.row(args, next)
			if err != nil {
				return nil, err
			}
			res.Rows = append(res.Rows, out)
		}
		return next, nil
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// ExecDelete applies a DELETE.
func ExecDelete(ctx context.Context, del *plan.Delete, args []types.Value) (Result, error) {
	pred, err := compilePredicate(del.Where)
	if err != nil {
		return Result{}, err
	}
	ret, err := compileReturning(del.Returning)
	if err != nil {
		return Result{}, err
	}

	var res Result
	err = del.Table.Mutate(func(row []types.Value) ([]types.Value, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match, err := pred(args, row)
		if err != nil {
			return nil, err
		}
		if !match {
			return row, nil
		}
		res.Affected++
		if ret != nil {
			// RETURNING on a DELETE reports the row as it was before removal.
			out, err := ret.row(args, row)
			if err != nil {
				return nil, err
			}
			res.Rows = append(res.Rows, out)
		}
		return nil, nil // delete
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// compilePredicate turns an optional WHERE into a row test. A missing clause
// matches every row, and a clause evaluating to unknown matches none — the same
// rule a Filter operator applies.
func compilePredicate(e plan.Expr) (func(args []types.Value, row Row) (bool, error), error) {
	if e == nil {
		return func([]types.Value, Row) (bool, error) { return true, nil }, nil
	}
	eval, err := Compile(e)
	if err != nil {
		return nil, err
	}
	return func(args []types.Value, row Row) (bool, error) {
		v, err := eval(args, row)
		if err != nil {
			return false, err
		}
		return v.Truth().IsTrue(), nil
	}, nil
}

// ExecInsert runs an INSERT.
func ExecInsert(ctx context.Context, ins *plan.Insert, args []types.Value) (Result, error) {
	width := len(ins.Table.Columns)
	ret, err := compileReturning(ins.Returning)
	if err != nil {
		return Result{}, err
	}

	var res Result
	for _, exprs := range ins.Rows {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		// Columns the statement did not name start as NULL, then serials are
		// filled from their sequence.
		row := make(Row, width)
		for i := range row {
			row[i] = types.Null()
		}
		for i, e := range exprs {
			eval, err := Compile(e)
			if err != nil {
				return res, err
			}
			v, err := eval(args, nil)
			if err != nil {
				return res, err
			}
			row[ins.Targets[i]] = v
		}
		for _, ord := range ins.Serials {
			row[ord] = types.Int(ins.Table.NextSerial(ord))
		}

		if err := ins.Table.Insert(row); err != nil {
			return res, err
		}
		res.Affected++

		// RETURNING is evaluated after the row is complete, so a generated
		// serial is visible to it.
		if ret != nil {
			out, err := ret.row(args, row)
			if err != nil {
				return res, err
			}
			res.Rows = append(res.Rows, out)
		}
	}
	return res, nil
}

// ExecCreateTable runs a CREATE TABLE.
func ExecCreateTable(cat *catalog.Catalog, ct *plan.CreateTable) error {
	_, err := cat.CreateTable(ct.Table, ct.IfNotExists)
	return err
}
