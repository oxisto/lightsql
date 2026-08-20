package driver_test

import (
	"slices"
	"strings"
	"testing"
)

// derivedFixture is deliberately the join fixture's shape, so that a derived
// table can be checked against the equivalent query written without one.
var derivedFixture = []string{
	`CREATE TABLE emp (id INT, name TEXT, dept INT)`,
	`CREATE TABLE dept (id INT, label TEXT)`,
	`INSERT INTO emp VALUES (1, 'ada', 10), (2, 'bob', NULL), (3, 'cy', 20)`,
	`INSERT INTO dept VALUES (10, 'eng'), (20, 'ops'), (30, 'hr')`,
}

func TestDerivedTable(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, derivedFixture...)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			// The simplest form: the derived table is the whole FROM clause,
			// so its ordinals are the outer row's ordinals unchanged.
			name:  "select star",
			query: `SELECT * FROM (SELECT name FROM emp) x ORDER BY name`,
			want:  []string{"ada", "bob", "cy"},
		},
		{
			// Referencing a derived column by the alias of the derived table,
			// which is the name the subquery's select list gave it.
			name:  "qualified reference",
			query: `SELECT x.name FROM (SELECT name FROM emp) x ORDER BY x.name`,
			want:  []string{"ada", "bob", "cy"},
		},
		{
			// An output alias inside the subquery is the derived column's name;
			// the underlying column name is not visible outside.
			name:  "inner alias names the column",
			query: `SELECT x.who FROM (SELECT name AS who FROM emp) x ORDER BY x.who`,
			want:  []string{"ada", "bob", "cy"},
		},
		{
			// A filter inside the subquery and another outside it must both
			// apply, and the outer one addresses the derived row.
			name:  "filter inside and outside",
			query: `SELECT n FROM (SELECT name AS n, dept FROM emp WHERE dept IS NOT NULL) x WHERE n <> 'ada'`,
			want:  []string{"cy"},
		},
		{
			// Joining a derived table to a base table. This is the case that
			// exercises ordinal offsetting: the derived side is two columns
			// wide, so dept's columns start at 2.
			name:  "joined to a base table",
			query: `SELECT x.name, dept.label FROM (SELECT name, dept FROM emp) x JOIN dept ON x.dept = dept.id ORDER BY x.name`,
			want:  []string{"ada|eng", "cy|ops"},
		},
		{
			// The derived table on the right of the join, so the offset applies
			// to it instead.
			name:  "derived on the right",
			query: `SELECT emp.name, d.label FROM emp JOIN (SELECT id, label FROM dept) d ON emp.dept = d.id ORDER BY emp.name`,
			want:  []string{"ada|eng", "cy|ops"},
		},
		{
			// Two derived tables joined to each other: both sides are subplans,
			// and neither may see the other's columns.
			name:  "two derived tables",
			query: `SELECT a.name, b.label FROM (SELECT name, dept FROM emp) a JOIN (SELECT id, label FROM dept) b ON a.dept = b.id ORDER BY a.name`,
			want:  []string{"ada|eng", "cy|ops"},
		},
		{
			// Nesting. The middle level must bind against the innermost one's
			// output, not against the base table.
			name:  "nested two deep",
			query: `SELECT y.n FROM (SELECT n FROM (SELECT name AS n FROM emp) x) y ORDER BY y.n`,
			want:  []string{"ada", "bob", "cy"},
		},
		{
			// A comma in FROM is a cross join, and a derived table is a valid
			// operand of one.
			name:  "comma join with a derived table",
			query: `SELECT x.name, dept.label FROM (SELECT name FROM emp WHERE name = 'ada') x, dept ORDER BY dept.label`,
			want:  []string{"ada|eng", "ada|hr", "ada|ops"},
		},
		{
			// Aggregation inside the subquery: the derived column is the
			// aggregate's result, and the outer query treats it as an ordinary
			// column rather than an aggregate.
			name:  "aggregate inside",
			query: `SELECT x.c FROM (SELECT count(*) AS c FROM emp) x`,
			want:  []string{"3"},
		},
		{
			// Grouping inside, filtering outside. This is the canonical reason
			// derived tables exist: it filters on an aggregate without HAVING.
			name:  "group inside, filter outside",
			query: `SELECT d, c FROM (SELECT dept AS d, count(*) AS c FROM emp GROUP BY dept) x WHERE c = 1 AND d IS NOT NULL ORDER BY d`,
			want:  []string{"10|1", "20|1"},
		},
		{
			// ORDER BY and LIMIT inside a derived table apply to the subquery,
			// so the outer query sees only the rows that survived.
			name:  "order and limit inside",
			query: `SELECT n FROM (SELECT name AS n FROM emp ORDER BY name LIMIT 2) x ORDER BY n`,
			want:  []string{"ada", "bob"},
		},
		{
			// A computed column with no alias takes PostgreSQL's ?column?
			// fallback, and is still addressable by position through *.
			name:  "computed column without an alias",
			query: `SELECT * FROM (SELECT id * 10 FROM emp) x ORDER BY 1`,
			want:  []string{"10", "20", "30"},
		},
		{
			// DISTINCT inside the subquery, to confirm the whole clause stack
			// is bound for a derived table and not just the simple parts.
			name:  "distinct inside",
			query: `SELECT dp FROM (SELECT DISTINCT dept AS dp FROM emp WHERE dept IS NOT NULL) x ORDER BY dp`,
			want:  []string{"10", "20"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rowsOf(t, db, tt.query); !slices.Equal(got, tt.want) {
				t.Errorf("%s\n got: %v\nwant: %v", tt.query, got, tt.want)
			}
		})
	}
}

// TestDerivedTableErrors covers the rules a derived table adds, each of which
// is a case where the query would otherwise resolve against a row the subplan
// never produces.
func TestDerivedTableErrors(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, derivedFixture...)

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			// PostgreSQL requires the alias, because the columns would
			// otherwise have nothing to be qualified by.
			name:  "missing alias",
			query: `SELECT * FROM (SELECT name FROM emp)`,
			want:  "subquery in FROM must have an alias",
		},
		{
			// A column the subquery did not select is not in scope outside it,
			// even though the base table has it.
			name:  "column not in the select list",
			query: `SELECT x.dept FROM (SELECT name FROM emp) x`,
			want:  "does not exist",
		},
		{
			// The base table's name is hidden by the alias, exactly as it is
			// for an aliased base table.
			name:  "base table name is hidden",
			query: `SELECT emp.name FROM (SELECT name FROM emp) x`,
			want:  "does not exist",
		},
		{
			// Without LATERAL a derived table cannot see the tables beside it.
			// This is the one that would silently "work" if the subquery were
			// bound in the outer scope — and would then read an ordinal
			// belonging to a row the subplan never sees.
			name:  "no implicit lateral",
			query: `SELECT x.label FROM emp, (SELECT label FROM dept WHERE id = emp.dept) x`,
			want:  "does not exist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Query(tt.query)
			if err == nil {
				t.Fatalf("%s: expected an error", tt.query)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s\n got: %v\nwant it to contain: %q", tt.query, err, tt.want)
			}
		})
	}
}
