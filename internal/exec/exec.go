package exec

import (
	"context"
	"math"

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

// ExecInsert runs an INSERT and reports how many rows it added.
func ExecInsert(ctx context.Context, ins *plan.Insert, args []types.Value) (int64, error) {
	width := len(ins.Table.Columns)

	var n int64
	for _, exprs := range ins.Rows {
		if err := ctx.Err(); err != nil {
			return n, err
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
				return n, err
			}
			v, err := eval(args, nil)
			if err != nil {
				return n, err
			}
			row[ins.Targets[i]] = v
		}
		for _, ord := range ins.Serials {
			row[ord] = types.Int(ins.Table.NextSerial(ord))
		}

		if err := ins.Table.Insert(row); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ExecCreateTable runs a CREATE TABLE.
func ExecCreateTable(cat *catalog.Catalog, ct *plan.CreateTable) error {
	_, err := cat.CreateTable(ct.Table, ct.IfNotExists)
	return err
}
