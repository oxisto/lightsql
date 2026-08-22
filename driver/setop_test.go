package driver_test

import (
	"slices"
	"strings"
	"testing"
)

var setOpFixture = []string{
	`CREATE TABLE a (v INT)`,
	`CREATE TABLE b (v INT)`,
	`INSERT INTO a VALUES (1), (1), (1), (2), (3), (NULL)`,
	`INSERT INTO b VALUES (1), (3), (4), (NULL)`,
}

// TestSetOperations covers all three, and the ALL forms specifically, since
// those are multiset operations rather than set ones: how many copies each side
// had decides the answer, not merely whether it had one.
func TestSetOperations(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, setOpFixture...)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"union removes duplicates", `SELECT v FROM a UNION SELECT v FROM b ORDER BY v`,
			[]string{"1", "2", "3", "4", "NULL"}},
		{"union all keeps them", `SELECT count(*) FROM (SELECT v FROM a UNION ALL SELECT v FROM b) u`,
			[]string{"10"}},
		{"intersect", `SELECT v FROM a INTERSECT SELECT v FROM b ORDER BY v`,
			[]string{"1", "3", "NULL"}},
		// a has three 1s and b has one, so one survives.
		{"intersect all takes the smaller count",
			`SELECT v FROM a INTERSECT ALL SELECT v FROM b ORDER BY v`,
			[]string{"1", "3", "NULL"}},
		{"except", `SELECT v FROM a EXCEPT SELECT v FROM b ORDER BY v`,
			[]string{"2"}},
		// Each row on the right cancels one on the left, not all of them: two
		// of a's three 1s survive.
		{"except all subtracts counts",
			`SELECT v FROM a EXCEPT ALL SELECT v FROM b ORDER BY v`,
			[]string{"1", "1", "2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rowsOf(t, db, tt.query); !slices.Equal(got, tt.want) {
				t.Errorf("%s\n got: %v\nwant: %v", tt.query, got, tt.want)
			}
		})
	}
}

// TestSetOperationsMatchNulls pins that a set operation treats two NULLs as the
// same row. That is the grouping rule rather than the comparison rule -- `NULL =
// NULL` is unknown, but GROUP BY and DISTINCT put NULLs together, and so does
// this. Getting it wrong gives a UNION that keeps every NULL it is given.
func TestSetOperationsMatchNulls(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, setOpFixture...)

	if got := rowsOf(t, db, `SELECT count(*) FROM (SELECT v FROM a UNION SELECT v FROM b) u WHERE v IS NULL`); !slices.Equal(got, []string{"1"}) {
		t.Errorf("a union of two nulls kept %v of them, want 1", got)
	}
	if got := rowsOf(t, db, `SELECT count(*) FROM (SELECT v FROM a INTERSECT SELECT v FROM b) u WHERE v IS NULL`); !slices.Equal(got, []string{"1"}) {
		t.Errorf("nulls did not intersect: %v", got)
	}
}

// TestSetOperationPrecedence pins the associativity, which is not what reading
// left to right suggests.
func TestSetOperationPrecedence(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE p (v INT)`, `CREATE TABLE q (v INT)`, `CREATE TABLE r (v INT)`,
		`INSERT INTO p VALUES (1), (2)`,
		`INSERT INTO q VALUES (2), (3)`,
		`INSERT INTO r VALUES (3), (4)`,
	)

	// INTERSECT binds tighter: this is p UNION (q INTERSECT r), which is
	// {1,2} UNION {3} = {1,2,3}. Read left to right it would be
	// (p UNION q) INTERSECT r = {3}.
	got := rowsOf(t, db, `SELECT v FROM p UNION SELECT v FROM q INTERSECT SELECT v FROM r ORDER BY v`)
	if want := []string{"1", "2", "3"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v -- INTERSECT should bind tighter than UNION", got, want)
	}

	// Parentheses override it.
	got = rowsOf(t, db, `(SELECT v FROM p UNION SELECT v FROM q) INTERSECT SELECT v FROM r ORDER BY v`)
	if want := []string{"3"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestSetOperationClauses covers the trailing clauses applying to the whole
// operation rather than to the arm they follow.
func TestSetOperationClauses(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE a2 (v INT)`, `CREATE TABLE b2 (v INT)`,
		`INSERT INTO a2 VALUES (3), (1)`,
		`INSERT INTO b2 VALUES (2), (4)`,
	)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"order by descending", `SELECT v FROM a2 UNION SELECT v FROM b2 ORDER BY v DESC`,
			[]string{"4", "3", "2", "1"}},
		{"order by position with a limit",
			`SELECT v FROM a2 UNION SELECT v FROM b2 ORDER BY 1 LIMIT 2`,
			[]string{"1", "2"}},
		{"offset", `SELECT v FROM a2 UNION SELECT v FROM b2 ORDER BY v OFFSET 2`,
			[]string{"3", "4"}},
		// The output column's name comes from the left arm, and ORDER BY may
		// use it.
		{"order by the output name", `SELECT v AS n FROM a2 UNION SELECT v FROM b2 ORDER BY n`,
			[]string{"1", "2", "3", "4"}},
		// A parenthesised arm carries its own, which is the only way to limit
		// one side.
		{"a parenthesised arm has its own limit",
			`(SELECT v FROM a2 ORDER BY v LIMIT 1) UNION (SELECT v FROM b2 ORDER BY v LIMIT 1) ORDER BY v`,
			[]string{"1", "2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rowsOf(t, db, tt.query); !slices.Equal(got, tt.want) {
				t.Errorf("%s\n got: %v\nwant: %v", tt.query, got, tt.want)
			}
		})
	}
}

// TestSetOperationsCompose covers a set operation appearing wherever a SELECT
// can. That is the reason it is a Query rather than a second kind of statement:
// every position accepts either without asking which it got.
func TestSetOperationsCompose(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE c1 (v INT)`, `CREATE TABLE c2 (v INT)`, `CREATE TABLE dest (v INT)`,
		`INSERT INTO c1 VALUES (1), (2)`,
		`INSERT INTO c2 VALUES (2), (3)`,
	)

	if got := rowsOf(t, db, `SELECT v FROM (SELECT v FROM c1 UNION SELECT v FROM c2) u WHERE v > 1 ORDER BY v`); !slices.Equal(got, []string{"2", "3"}) {
		t.Errorf("derived table: %v", got)
	}
	if got := rowsOf(t, db, `SELECT v FROM c1 WHERE v IN (SELECT v FROM c1 INTERSECT SELECT v FROM c2) ORDER BY v`); !slices.Equal(got, []string{"2"}) {
		t.Errorf("subquery: %v", got)
	}
	mustExecAll(t, db, `INSERT INTO dest SELECT v FROM c1 UNION SELECT v FROM c2`)
	if got := rowsOf(t, db, `SELECT v FROM dest ORDER BY v`); !slices.Equal(got, []string{"1", "2", "3"}) {
		t.Errorf("insert from a set operation: %v", got)
	}
	if got := rowsOf(t, db, `SELECT v, count(*) FROM (SELECT v FROM c1 UNION ALL SELECT v FROM c2) u GROUP BY v ORDER BY v`); !slices.Equal(got, []string{"1|1", "2|2", "3|1"}) {
		t.Errorf("grouping over a set operation: %v", got)
	}
}

// TestSetOperationErrors covers the two ways the arms can fail to agree.
func TestSetOperationErrors(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE e1 (v INT)`,
		`CREATE TABLE e2 (s TEXT)`,
	)

	tests := []struct {
		name string
		stmt string
		want string
	}{
		{"different column counts", `SELECT v FROM e1 UNION SELECT v, v FROM e1`,
			"same number of columns"},
		{"types that cannot be matched", `SELECT v FROM e1 UNION SELECT s FROM e2`,
			"cannot be matched"},
		// ORDER BY after a set operation may only name an output column: the
		// arms are separate queries with separate scopes, so a term naming a
		// column of one of them has no single meaning.
		{"order by an input column", `SELECT v FROM e1 UNION SELECT v FROM e1 ORDER BY e1.v`,
			"must name an output column"},
		{"order by past the end", `SELECT v FROM e1 UNION SELECT v FROM e1 ORDER BY 2`,
			"not in the select list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := queryErr(db, tt.stmt)
			if err == nil {
				t.Fatalf("%s: expected an error", tt.stmt)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s\n got: %v\nwant it to contain: %q", tt.stmt, err, tt.want)
			}
		})
	}
}
