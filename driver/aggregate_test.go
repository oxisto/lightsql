package driver_test

import (
	"database/sql"
	"slices"
	"testing"
)

var aggFixture = []string{
	`CREATE TABLE sales (region TEXT, amount INT, rate DOUBLE PRECISION)`,
	`INSERT INTO sales VALUES
		('east', 10, 1.5),
		('east', 20, 2.5),
		('west', 5,  0.5),
		('west', NULL, NULL),
		(NULL,   7,  1.0)`,
}

func TestAggregatesWithoutGroupBy(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, aggFixture...)

	tests := []struct {
		name  string
		query string
		want  string
	}{
		// count(*) counts rows; count(x) counts values that are not NULL. The
		// difference is the whole reason both spellings exist.
		{"count star counts rows", `SELECT count(*) FROM sales`, "5"},
		{"count of a column skips NULL", `SELECT count(amount) FROM sales`, "4"},
		{"sum skips NULL", `SELECT sum(amount) FROM sales`, "42"},
		{"min skips NULL", `SELECT min(amount) FROM sales`, "5"},
		{"max skips NULL", `SELECT max(amount) FROM sales`, "20"},
		{"avg is float even over integers", `SELECT avg(amount) FROM sales`, "10.5"},
		{"sum over floats stays float", `SELECT sum(rate) FROM sales`, "5.5"},
		{"several aggregates in one row", `SELECT count(*), sum(amount) FROM sales`, "5|42"},
		// An aggregate over an expression, not just a column.
		{"aggregate of an expression", `SELECT sum(amount * 2) FROM sales`, "84"},
		// An expression over an aggregate, which is the other direction.
		{"expression over an aggregate", `SELECT sum(amount) + 1 FROM sales`, "43"},
		{"distinct", `SELECT count(DISTINCT region) FROM sales`, "2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rowsOf(t, db, tt.query)
			if !slices.Equal(got, []string{tt.want}) {
				t.Errorf("got %v, want [%s]", got, tt.want)
			}
		})
	}
}

// TestAggregatesOverNoRows is the rule that catches people out: count answers
// zero, everything else answers NULL, and a query with no GROUP BY still
// produces exactly one row.
func TestAggregatesOverNoRows(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, `CREATE TABLE empty (n INT)`)

	got := rowsOf(t, db, `SELECT count(*), count(n), sum(n), avg(n), min(n), max(n) FROM empty`)
	want := []string{"0|0|NULL|NULL|NULL|NULL"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// With a GROUP BY there are no groups, so there are no rows at all.
	got = rowsOf(t, db, `SELECT n, count(*) FROM empty GROUP BY n`)
	if len(got) != 0 {
		t.Errorf("got %v, want no rows", got)
	}
}

func TestGroupBy(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, aggFixture...)

	t.Run("groups and counts", func(t *testing.T) {
		got := rowsOf(t, db, `
			SELECT region, count(*), sum(amount) FROM sales
			GROUP BY region ORDER BY region`)
		// NULL is one group, not one group per NULL, and sorts last.
		want := []string{"east|2|30", "west|2|5", "NULL|1|7"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("group by an expression", func(t *testing.T) {
		mustExecAll(t, db, `CREATE TABLE n (v INT)`,
			`INSERT INTO n VALUES (1), (2), (3), (4), (5)`)
		got := rowsOf(t, db, `SELECT v % 2, count(*) FROM n GROUP BY v % 2 ORDER BY v % 2`)
		if want := []string{"0|2", "1|3"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("group by several columns", func(t *testing.T) {
		mustExecAll(t, db, `CREATE TABLE pair (a TEXT, b TEXT)`,
			`INSERT INTO pair VALUES ('x','1'), ('x','1'), ('x','2'), ('y','1')`)
		got := rowsOf(t, db, `SELECT a, b, count(*) FROM pair GROUP BY a, b ORDER BY a, b`)
		want := []string{"x|1|2", "x|2|1", "y|1|1"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("a grouped column need not be selected", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT count(*) FROM sales GROUP BY region ORDER BY count(*)`)
		if want := []string{"1", "2", "2"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestHaving(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, aggFixture...)

	t.Run("filters on an aggregate", func(t *testing.T) {
		got := rowsOf(t, db, `
			SELECT region FROM sales GROUP BY region
			HAVING count(*) > 1 ORDER BY region`)
		if want := []string{"east", "west"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("filters on a group key", func(t *testing.T) {
		got := rowsOf(t, db, `
			SELECT region, sum(amount) FROM sales GROUP BY region
			HAVING region = 'east'`)
		if want := []string{"east|30"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// HAVING runs after grouping, WHERE before it. This pair is the difference:
	// WHERE removes rows from the groups, HAVING removes whole groups.
	t.Run("WHERE removes rows, HAVING removes groups", func(t *testing.T) {
		got := rowsOf(t, db, `
			SELECT region, count(*) FROM sales WHERE amount > 5
			GROUP BY region ORDER BY region`)
		want := []string{"east|2", "NULL|1"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// An aggregate may appear in HAVING without appearing in the select list.
	t.Run("aggregate only in HAVING", func(t *testing.T) {
		got := rowsOf(t, db, `
			SELECT region FROM sales GROUP BY region
			HAVING sum(amount) > 10`)
		if want := []string{"east"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// TestOrderByAggregate checks that an aggregate first mentioned in ORDER BY is
// still collected, since the aggregate node is built only once every clause
// above it has been bound.
func TestOrderByAggregate(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, aggFixture...)

	got := rowsOf(t, db, `
		SELECT region FROM sales GROUP BY region ORDER BY sum(amount) DESC`)
	if want := []string{"east", "NULL", "west"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestAggregateWithJoin(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE emp (id INT, dept INT)`,
		`CREATE TABLE dept (id INT, label TEXT)`,
		`INSERT INTO emp VALUES (1, 10), (2, 10), (3, 20)`,
		`INSERT INTO dept VALUES (10, 'eng'), (20, 'ops'), (30, 'hr')`,
	)

	got := rowsOf(t, db, `
		SELECT dept.label, count(*) FROM dept JOIN emp ON emp.dept = dept.id
		GROUP BY dept.label ORDER BY dept.label`)
	if want := []string{"eng|2", "ops|1"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// A LEFT JOIN keeps hr, and count(emp.id) reports 0 for it while count(*)
	// would report 1 — the NULL-padded row is still a row.
	got = rowsOf(t, db, `
		SELECT dept.label, count(emp.id), count(*) FROM dept LEFT JOIN emp ON emp.dept = dept.id
		GROUP BY dept.label ORDER BY dept.label`)
	if want := []string{"eng|2|2", "hr|0|1", "ops|1|1"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestAggregateErrors(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, aggFixture...)

	tests := []struct {
		name  string
		query string
		state string
	}{
		{
			// The canonical mistake: a column that is neither grouped nor
			// aggregated has no single value for the group.
			name:  "ungrouped column",
			query: `SELECT region, amount FROM sales GROUP BY region`,
			state: "42803",
		},
		{
			name:  "ungrouped column with no GROUP BY at all",
			query: `SELECT region, count(*) FROM sales`,
			state: "42803",
		},
		{
			// WHERE runs before grouping, so an aggregate there has nothing to
			// aggregate over.
			name:  "aggregate in WHERE",
			query: `SELECT count(*) FROM sales WHERE count(*) > 1`,
			state: "42803",
		},
		{
			name:  "nested aggregate",
			query: `SELECT sum(count(*)) FROM sales`,
			state: "42803",
		},
		{
			name:  "unknown function",
			query: `SELECT nosuchfunc(amount) FROM sales`,
			state: "42883",
		},
		{
			name:  "sum takes one argument",
			query: `SELECT sum(amount, 1) FROM sales`,
			state: "42601",
		},
		{
			name:  "only count takes a star",
			query: `SELECT sum(*) FROM sales`,
			state: "42601",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Query(tt.query)
			if err == nil {
				t.Fatalf("%s was accepted", tt.query)
			}
			if got := sqlstate(err); got != tt.state {
				t.Errorf("SQLSTATE = %q, want %q (%v)", got, tt.state, err)
			}
		})
	}
}

// TestAggregateScansIntoNullable checks the driver side: an aggregate over no
// rows is NULL, and a caller scanning into a plain int64 must get a usable
// error rather than a zero.
func TestAggregateScansIntoNullable(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, `CREATE TABLE e (n INT)`)

	var sum sql.NullInt64
	if err := db.QueryRow(`SELECT sum(n) FROM e`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if sum.Valid {
		t.Errorf("sum over no rows = %d, want NULL", sum.Int64)
	}

	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM e`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("count over no rows = %d, want 0", n)
	}
}
