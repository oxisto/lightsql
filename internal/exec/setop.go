package exec

import (
	"context"

	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/builtin"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/types"
)

// setOpOp implements UNION, INTERSECT and EXCEPT.
//
// The left side streams; only the right is read into memory, and only when the
// operation needs to know what is on it. UNION ALL needs to know nothing, so it
// concatenates without materialising anything at all -- which matters because
// UNION ALL is the one people reach for in a loop.
//
// The rest build a count of the right side and then stream the left against it.
// Counting rather than merely recording presence is what makes the ALL forms
// multiset operations: with three copies on the left and one on the right,
// INTERSECT ALL yields one and EXCEPT ALL yields two.
type setOpOp struct {
	left, right Operator
	op          ast.SetOpKind
	all         bool

	// seen suppresses duplicates in the output for the distinct forms. A row
	// already emitted is not emitted again however many times it appears.
	seen *builtin.KeySet
	// counts holds the right side, for everything but UNION ALL.
	counts *builtin.KeyCounts
	loaded bool
	// draining is set once the left side is exhausted and UNION is working
	// through the right.
	draining bool
}

func newSetOp(left, right Operator, n *plan.SetOp) *setOpOp {
	o := &setOpOp{left: left, right: right, op: n.Op, all: n.All}
	if !n.All {
		o.seen = builtin.NewKeySet()
	}
	return o
}

func (o *setOpOp) Next(ctx context.Context) (Row, bool, error) {
	// Cancellation is checked per row rather than once, so a set operation over
	// two large inputs stops when its context does.
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	if o.op != ast.SetUnion && !o.loaded {
		if err := o.loadRight(ctx); err != nil {
			return nil, false, err
		}
	}

	for {
		row, ok, err := o.nextCandidate(ctx)
		if err != nil || !ok {
			return nil, false, err
		}
		if o.keep(row) {
			return row, true, nil
		}
	}
}

// nextCandidate returns the next row from whichever side is still producing.
// Only UNION reads the right side directly; the others have already consumed it
// into counts.
func (o *setOpOp) nextCandidate(ctx context.Context) (Row, bool, error) {
	if !o.draining {
		row, ok, err := o.left.Next(ctx)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return row, true, nil
		}
		o.draining = true
	}
	if o.op != ast.SetUnion {
		return nil, false, nil
	}
	return o.right.Next(ctx)
}

// keep decides whether a candidate row belongs in the output.
func (o *setOpOp) keep(row Row) bool {
	switch o.op {
	case ast.SetUnion:
		// Every row from either side, minus the duplicates unless ALL.
		return o.all || o.seen.Add(row)

	case ast.SetIntersect:
		if o.all {
			// One row on the left is matched by at most one on the right.
			return o.counts.Take(row)
		}
		return o.counts.Has(row) && o.seen.Add(row)

	default: // ast.SetExcept
		if o.all {
			// A row on the right cancels one on the left, not all of them.
			return !o.counts.Take(row)
		}
		return !o.counts.Has(row) && o.seen.Add(row)
	}
}

// loadRight reads the whole right side into counts.
//
// It is the one place this operator gives up streaming, and it has to: whether
// a left row belongs in the output depends on the right side as a whole, which
// is not knowable until all of it has been seen.
func (o *setOpOp) loadRight(ctx context.Context) error {
	o.counts = builtin.NewKeyCounts()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, ok, err := o.right.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			o.loaded = true
			return nil
		}
		o.counts.Add([]types.Value(row))
	}
}

func (o *setOpOp) Close() error {
	err := o.left.Close()
	if cerr := o.right.Close(); err == nil {
		err = cerr
	}
	return err
}
