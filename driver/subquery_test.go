package driver_test

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// subqueryFixture keeps a NULL on each side of the relationship: bob has no
// department, and hr has no employee. The NULL is what makes IN and NOT IN
// differ from a plain join, so it has to be in the data rather than in one
// bolted-on case.
var subqueryFixture = []string{
	`CREATE TABLE emp (id INT, name TEXT, dept INT)`,
	`CREATE TABLE dept (id INT, label TEXT)`,
	`INSERT INTO emp VALUES (1, 'ada', 10), (2, 'bob', NULL), (3, 'cy', 20)`,
	`INSERT INTO dept VALUES (10, 'eng'), (20, 'ops'), (30, 'hr')`,
}

func TestScalarSubquery(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, subqueryFixture...)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "in the select list",
			query: `SELECT (SELECT count(*) FROM dept)`,
			want:  []string{"3"},
		},
		{
			// Evaluated once and reused for every outer row, which is what
			// being uncorrelated buys.
			name:  "same value on every row",
			query: `SELECT name, (SELECT count(*) FROM dept) FROM emp ORDER BY name`,
			want:  []string{"ada|3", "bob|3", "cy|3"},
		},
		{
			name:  "as a comparison operand",
			query: `SELECT name FROM emp WHERE dept = (SELECT id FROM dept WHERE label = 'eng')`,
			want:  []string{"ada"},
		},
		{
			// No rows is NULL, not an error, and NULL fails the comparison so
			// no row survives.
			name:  "no rows yields null",
			query: `SELECT name FROM emp WHERE dept = (SELECT id FROM dept WHERE label = 'nope')`,
			want:  nil,
		},
		{
			name:  "no rows is null, not zero",
			query: `SELECT (SELECT id FROM dept WHERE label = 'nope') IS NULL`,
			want:  []string{"true"},
		},
		{
			// Arithmetic over a scalar subquery, to confirm it is an ordinary
			// operand and not a special case of comparison.
			name:  "inside an expression",
			query: `SELECT (SELECT count(*) FROM dept) * 2 + 1`,
			want:  []string{"7"},
		},
		{
			name:  "aggregate over a filtered subquery",
			query: `SELECT (SELECT max(id) FROM dept WHERE label <> 'hr')`,
			want:  []string{"20"},
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

func TestExistsSubquery(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, subqueryFixture...)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "matching rows",
			query: `SELECT name FROM emp WHERE EXISTS (SELECT 1 FROM dept) ORDER BY name`,
			want:  []string{"ada", "bob", "cy"},
		},
		{
			name:  "no matching rows",
			query: `SELECT name FROM emp WHERE EXISTS (SELECT 1 FROM dept WHERE id = 999)`,
			want:  nil,
		},
		{
			name:  "not exists",
			query: `SELECT name FROM emp WHERE NOT EXISTS (SELECT 1 FROM dept WHERE id = 999) ORDER BY name`,
			want:  []string{"ada", "bob", "cy"},
		},
		{
			// EXISTS ignores the select list entirely, so a wider subquery is
			// legal where a scalar one would be rejected.
			name:  "select list is irrelevant",
			query: `SELECT count(*) FROM emp WHERE EXISTS (SELECT id, label FROM dept)`,
			want:  []string{"3"},
		},
		{
			// A row of NULLs is still a row. EXISTS is the one subquery form
			// that never answers unknown.
			name:  "a null row still exists",
			query: `SELECT EXISTS (SELECT NULL FROM dept)`,
			want:  []string{"true"},
		},
		{
			name:  "exists over no rows is false, not null",
			query: `SELECT EXISTS (SELECT 1 FROM dept WHERE id = 999)`,
			want:  []string{"false"},
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

// TestInThreeValued is the part of IN that is easy to get wrong: without a
// match, a NULL among the candidates makes the answer unknown rather than
// false, because the missing value might have been the matching one.
func TestInThreeValued(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, subqueryFixture...)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			// emp.dept is {10, NULL, 20}. 10 and 20 match outright.
			name:  "in a subquery containing null",
			query: `SELECT label FROM dept WHERE id IN (SELECT dept FROM emp) ORDER BY label`,
			want:  []string{"eng", "ops"},
		},
		{
			// hr (30) matches nothing, but the NULL makes that unknown rather
			// than false, so NOT IN returns nothing at all. This is the rule
			// that surprises people, and the one worth pinning.
			name:  "not in a subquery containing null yields nothing",
			query: `SELECT label FROM dept WHERE id NOT IN (SELECT dept FROM emp)`,
			want:  nil,
		},
		{
			// With the NULL excluded, NOT IN behaves as expected again.
			name:  "not in, null excluded",
			query: `SELECT label FROM dept WHERE id NOT IN (SELECT dept FROM emp WHERE dept IS NOT NULL) ORDER BY label`,
			want:  []string{"hr"},
		},
		{
			// A NULL on the left is unknown whatever the candidates are.
			name:  "null on the left",
			query: `SELECT name FROM emp WHERE dept IN (SELECT id FROM dept) ORDER BY name`,
			want:  []string{"ada", "cy"},
		},
		{
			name:  "in a literal list",
			query: `SELECT name FROM emp WHERE id IN (1, 3) ORDER BY name`,
			want:  []string{"ada", "cy"},
		},
		{
			name:  "not in a literal list",
			query: `SELECT name FROM emp WHERE id NOT IN (1) ORDER BY name`,
			want:  []string{"bob", "cy"},
		},
		{
			// The same three-valued rule applies to a written list.
			name:  "literal list containing null",
			query: `SELECT name FROM emp WHERE id IN (1, NULL) ORDER BY name`,
			want:  []string{"ada"},
		},
		{
			name:  "not in a literal list containing null",
			query: `SELECT name FROM emp WHERE id NOT IN (1, NULL)`,
			want:  nil,
		},
		{
			// A list element may reference a column, so unlike a subquery the
			// list is evaluated per row.
			name:  "list referencing a column",
			query: `SELECT name FROM emp WHERE id IN (dept, 3) ORDER BY name`,
			want:  []string{"cy"},
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

func TestSubqueryErrors(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, subqueryFixture...)

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			// SQL does not pick one, because which one would depend on a row
			// order the query never asked for.
			name:  "scalar subquery returning several rows",
			query: `SELECT (SELECT id FROM dept)`,
			want:  "more than one row returned by a subquery used as an expression",
		},
		{
			name:  "scalar subquery returning several columns",
			query: `SELECT (SELECT id, label FROM dept)`,
			want:  "subquery must return only one column",
		},
		{
			name:  "IN subquery returning several columns",
			query: `SELECT name FROM emp WHERE dept IN (SELECT id, label FROM dept)`,
			want:  "subquery must return only one column",
		},
		{
			// Correlated subqueries are not supported yet. The reference must
			// fail rather than resolve against the subplan's own row.
			name:  "correlated reference is rejected",
			query: `SELECT name FROM emp WHERE EXISTS (SELECT 1 FROM dept WHERE dept.id = emp.dept)`,
			want:  "does not exist",
		},
		{
			// A list element is type-checked against the left operand, so this
			// is an error rather than a comparison that quietly matches nothing.
			name:  "type mismatch in a literal list",
			query: `SELECT name FROM emp WHERE id IN ('abc')`,
			want:  "invalid input syntax",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := queryErr(db, tt.query)
			if err == nil {
				t.Fatalf("%s: expected an error", tt.query)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s\n got: %v\nwant it to contain: %q", tt.query, err, tt.want)
			}
		})
	}
}

// queryErr runs a query to completion and reports the first error.
//
// The rows have to be drained rather than merely requested: a binding error
// surfaces from Query, but a cardinality violation is raised while a row is
// being evaluated, so checking only Query would call the second one a pass.
func queryErr(db *sql.DB, query string) error {
	rows, err := db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

// TestSubqueryInDML checks the places other than SELECT where a subquery is
// legal, since each one is a separate scope in the binder and it is the scope,
// not the expression, that decides whether a subquery can be resolved.
func TestSubqueryInDML(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, subqueryFixture...)

	if _, err := db.Exec(`INSERT INTO dept VALUES ((SELECT max(id) FROM dept) + 10, 'new')`); err != nil {
		t.Fatalf("INSERT with a scalar subquery: %v", err)
	}
	if got := rowsOf(t, db, `SELECT id, label FROM dept WHERE label = 'new'`); !slices.Equal(got, []string{"40|new"}) {
		t.Errorf("after INSERT: got %v", got)
	}

	if _, err := db.Exec(`UPDATE emp SET dept = (SELECT id FROM dept WHERE label = 'ops') WHERE name = 'bob'`); err != nil {
		t.Fatalf("UPDATE with a scalar subquery: %v", err)
	}
	if got := rowsOf(t, db, `SELECT dept FROM emp WHERE name = 'bob'`); !slices.Equal(got, []string{"20"}) {
		t.Errorf("after UPDATE: got %v", got)
	}

	if _, err := db.Exec(`DELETE FROM emp WHERE dept IN (SELECT id FROM dept WHERE label = 'eng')`); err != nil {
		t.Fatalf("DELETE with an IN subquery: %v", err)
	}
	if got := rowsOf(t, db, `SELECT name FROM emp ORDER BY name`); !slices.Equal(got, []string{"bob", "cy"}) {
		t.Errorf("after DELETE: got %v", got)
	}
}

// TestSubqueryNotAllowed covers the scopes deliberately left without a binder,
// where SQL has no row for a subquery to run against.
func TestSubqueryNotAllowed(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, subqueryFixture...)

	stmts := []string{
		`CREATE TABLE bad1 (n INT CHECK (n < (SELECT max(id) FROM dept)))`,
		`CREATE TABLE bad2 (n INT DEFAULT (SELECT max(id) FROM dept))`,
	}
	for _, s := range stmts {
		t.Run(s[:24], func(t *testing.T) {
			if _, err := db.Exec(s); err == nil {
				t.Errorf("%s: expected an error", s)
			} else if !strings.Contains(err.Error(), "subquery") {
				t.Errorf("%s: got %v, want it to mention a subquery", s, err)
			}
		})
	}
}
