package driver_test

import (
	"slices"
	"testing"
)

var distinctFixture = []string{
	`CREATE TABLE t (a INT, b TEXT)`,
	`INSERT INTO t VALUES
		(1, 'x'), (1, 'y'), (1, 'x'),
		(2, 'z'),
		(NULL, 'n'), (NULL, 'n')`,
}

func TestDistinct(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, distinctFixture...)

	t.Run("on one column", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT DISTINCT a FROM t ORDER BY a`)
		// The two NULL rows collapse into one: DISTINCT treats NULLs as equal
		// even though NULL = NULL is unknown.
		if want := []string{"1", "2", "NULL"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("on the whole row", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT DISTINCT a, b FROM t ORDER BY a, b`)
		want := []string{"1|x", "1|y", "2|z", "NULL|n"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("deduplicates on the output, not the input", func(t *testing.T) {
		// The rows differ in b, so a naive dedupe over input rows would keep
		// all three. DISTINCT compares what the query returns.
		got := rowsOf(t, db, `SELECT DISTINCT a FROM t WHERE a = 1`)
		if want := []string{"1"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("over an expression", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT DISTINCT a * 2 FROM t ORDER BY a * 2`)
		if want := []string{"2", "4", "NULL"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("with LIMIT", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT DISTINCT a FROM t ORDER BY a LIMIT 2`)
		if want := []string{"1", "2"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("with an aggregate", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT DISTINCT count(*) FROM t GROUP BY a ORDER BY count(*)`)
		// Groups are 1 (3 rows), 2 (1 row), NULL (2 rows) -> counts 3,1,2,
		// which are already distinct, so this checks the two features compose
		// rather than that anything collapses.
		if want := []string{"1", "2", "3"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestDistinctOn(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, distinctFixture...)

	t.Run("keeps the first row of each key", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT DISTINCT ON (a) a, b FROM t ORDER BY a, b`)
		// Within a = 1 the rows sort x, x, y, so x wins.
		want := []string{"1|x", "2|z", "NULL|n"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("the ORDER BY decides which row wins", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT DISTINCT ON (a) a, b FROM t ORDER BY a, b DESC`)
		// Sorting b downwards makes y the first row for a = 1 instead.
		want := []string{"1|y", "2|z", "NULL|n"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("the key need not be selected", func(t *testing.T) {
		// b is the only output column, but uniqueness is decided by a. This is
		// the case that needs the projection to carry an extra column and the
		// distinct to trim it off again.
		got := rowsOf(t, db, `SELECT DISTINCT ON (a) b FROM t ORDER BY a, b`)
		if want := []string{"x", "z", "n"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("several key expressions", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT DISTINCT ON (a, b) a, b FROM t ORDER BY a, b`)
		want := []string{"1|x", "1|y", "2|z", "NULL|n"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("the column list is the select list", func(t *testing.T) {
		rows, err := db.Query(`SELECT DISTINCT ON (a) b FROM t ORDER BY a`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		// The scaffolding column for a must not show up in the result.
		if want := []string{"b"}; !slices.Equal(cols, want) {
			t.Errorf("columns = %v, want %v", cols, want)
		}
	})
}
