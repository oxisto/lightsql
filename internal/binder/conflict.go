package binder

import (
	"slices"

	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
)

// excludedAlias is the name the proposed row is reached under inside DO UPDATE.
// PostgreSQL fixes it, so it is not configurable and not a table that can be
// shadowed.
const excludedAlias = "excluded"

// bindOnConflict resolves an ON CONFLICT clause.
//
// The arbiter columns must be covered by a uniqueness constraint, and that is
// checked rather than assumed: a target naming columns nothing enforces would
// never detect a conflict, so the statement would silently behave like a plain
// INSERT and fail later on some other constraint.
func (b *Binder) bindOnConflict(c *ast.OnConflictClause, t *catalog.Table) (*plan.OnConflict, error) {
	out := &plan.OnConflict{}

	for _, name := range c.Target {
		i := t.ColumnIndex(name.Name)
		if i < 0 {
			return nil, pgerr.Newf(pgerr.UndefinedColumn,
				"column %q of relation %q does not exist", name.Name, t.Name).At(name.Pos())
		}
		out.Arbiter = append(out.Arbiter, i)
	}

	if len(out.Arbiter) > 0 && !uniquelyConstrained(t, out.Arbiter) {
		return nil, pgerr.New(pgerr.SyntaxError,
			"there is no unique or exclusion constraint matching the ON CONFLICT specification").
			At(c.Pos())
	}

	if c.DoUpdate == nil {
		out.DoNothing = true
		return out, nil
	}

	// DO UPDATE needs to know which row it is changing, so it cannot yield to
	// "whichever constraint happened to fire".
	if len(out.Arbiter) == 0 {
		return nil, pgerr.New(pgerr.SyntaxError,
			"ON CONFLICT DO UPDATE requires a conflict target").At(c.Pos())
	}

	// The scope is the stored row followed by the proposed one, which is the
	// same concatenation a join produces -- so `excluded.x` is an ordinal like
	// any other and nothing below the binder knows the word.
	sc := &scope{b: b}
	sc.addTable(t, t.Name)
	sc.addTable(t, excludedAlias)

	seen := make(map[int]bool, len(c.DoUpdate))
	for _, a := range c.DoUpdate {
		i := t.ColumnIndex(a.Column.Name)
		if i < 0 {
			return nil, pgerr.Newf(pgerr.UndefinedColumn,
				"column %q of relation %q does not exist", a.Column.Name, t.Name).At(a.Column.Pos())
		}
		if seen[i] {
			return nil, pgerr.Newf(pgerr.SyntaxError,
				"multiple assignments to same column %q", a.Column.Name).At(a.Column.Pos())
		}
		seen[i] = true

		val, err := bindExpr(a.Value, sc)
		if err != nil {
			return nil, err
		}
		if val, err = coerce(val, t.Columns[i].Type.Kind, a.Value.Pos()); err != nil {
			return nil, err
		}
		out.Assignments = append(out.Assignments, plan.Assignment{Ordinal: i, Value: val})
	}

	var err error
	if out.Where, err = b.bindPredicate(c.Where, sc, "ON CONFLICT DO UPDATE WHERE"); err != nil {
		return nil, err
	}
	return out, nil
}

// uniquelyConstrained reports whether some primary key, unique constraint or
// unique index covers exactly the given columns.
//
// Order does not matter -- UNIQUE (a, b) constrains the same thing as
// UNIQUE (b, a) -- so the comparison is over the set.
func uniquelyConstrained(t *catalog.Table, cols []int) bool {
	want := slices.Sorted(slices.Values(cols))

	for _, c := range t.Constraints {
		if slices.Equal(slices.Sorted(slices.Values(c.Columns)), want) {
			return true
		}
	}
	for _, ix := range t.Indexes {
		// A partial index only constrains the rows it covers, so it cannot
		// serve as an arbiter for rows the statement has not looked at yet.
		if !ix.Unique || ix.Where != nil {
			continue
		}
		if slices.Equal(slices.Sorted(slices.Values(ix.Columns)), want) {
			return true
		}
	}
	return false
}
