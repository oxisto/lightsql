package exec

import (
	"context"

	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/storage"
	"github.com/oxisto/lightsql/internal/types"
)

// conflictHandler carries out an ON CONFLICT clause.
type conflictHandler struct {
	arbiter   []int
	doNothing bool
	assign    []Eval
	targets   []int
	where     func(ctx context.Context, args []types.Value, row Row) (bool, error)
	// joined is the row the assignments are evaluated over: the stored row
	// followed by the proposed one. It is reused across rows, since only one
	// conflict is resolved at a time.
	joined Row
	width  int
}

func compileConflict(ins *plan.Insert, tx *storage.Tx) (*conflictHandler, error) {
	c := ins.OnConflict
	if c == nil {
		return nil, nil
	}

	width := len(ins.Table.Columns)
	h := &conflictHandler{
		arbiter:   c.Arbiter,
		doNothing: c.DoNothing,
		width:     width,
		joined:    make(Row, 2*width),
	}
	for _, a := range c.Assignments {
		eval, err := Compile(a.Value, tx)
		if err != nil {
			return nil, err
		}
		h.assign = append(h.assign, eval)
		h.targets = append(h.targets, a.Ordinal)
	}
	pred, err := compilePredicate(c.Where, tx)
	if err != nil {
		return nil, err
	}
	h.where = pred
	return h, nil
}

// find returns the stored version the proposed row collides with, or nil.
//
// With no arbiter -- a bare DO NOTHING -- every uniqueness constraint is
// consulted, because the clause yields to whichever one would have fired.
func (h *conflictHandler) find(tx *storage.Tx, t *catalog.Table, row Row) *storage.Version {
	keys := [][]int{h.arbiter}
	if len(h.arbiter) == 0 {
		keys = keys[:0]
		for _, c := range t.Constraints {
			keys = append(keys, c.Columns)
		}
		for _, ix := range t.Indexes {
			// A partial index covers only some rows, so it cannot decide a
			// collision for a row the statement has not evaluated it against.
			if ix.Unique && ix.Where == nil {
				keys = append(keys, ix.Columns)
			}
		}
	}

	for _, cols := range keys {
		// A NULL is never equal to anything, so a proposed row with a NULL in a
		// key column cannot collide on it -- the same rule that lets UNIQUE
		// permit any number of NULLs.
		if anyNullAt(row, cols) {
			continue
		}
		for _, v := range t.Scan(tx) {
			if anyNullAt(v.Vals, cols) {
				continue
			}
			if sameAt(v.Vals, row, cols) {
				return v
			}
		}
	}
	return nil
}

// resolve applies the clause to a collision that find located.
func (h *conflictHandler) resolve(
	ctx context.Context,
	tx *storage.Tx,
	t *catalog.Table,
	existing *storage.Version,
	proposed Row,
	args []types.Value,
	checks []compiledCheck,
	ret *returningEval,
	res *Result,
) error {
	if h.doNothing {
		// Not an error and not a row: PostgreSQL reports zero rows affected,
		// which is how a caller tells a skip from a write.
		return nil
	}

	copy(h.joined, existing.Vals)
	copy(h.joined[h.width:], proposed)

	ok, err := h.where(ctx, args, h.joined)
	if err != nil {
		return err
	}
	if !ok {
		// The predicate excluded this row, so the update does not happen -- and
		// neither does the insert, since the row is still there.
		return nil
	}

	next := make(Row, h.width)
	copy(next, existing.Vals)
	for i, eval := range h.assign {
		v, err := eval(ctx, args, h.joined)
		if err != nil {
			return err
		}
		next[h.targets[i]] = v
	}

	if err := runChecks(ctx, checks, args, next, t.Name); err != nil {
		return err
	}
	if err := t.Update(tx, existing, next); err != nil {
		return err
	}
	res.Affected++

	// RETURNING reports the row as it now stands, which for DO UPDATE is the
	// updated one rather than the one that was proposed.
	if ret != nil {
		out, err := ret.row(ctx, args, next)
		if err != nil {
			return err
		}
		res.Rows = append(res.Rows, out)
	}
	return nil
}

func anyNullAt(row []types.Value, cols []int) bool {
	for _, ord := range cols {
		if row[ord].IsNull() {
			return true
		}
	}
	return false
}

// sameAt compares two rows on the given columns using the grouping form of
// equality, which is what a uniqueness check uses. Only non-NULL keys reach
// here.
func sameAt(a, b []types.Value, cols []int) bool {
	for _, ord := range cols {
		if !types.Equal(a[ord], b[ord]) {
			return false
		}
	}
	return true
}
