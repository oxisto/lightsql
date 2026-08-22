package driver_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	lightsqldriver "github.com/oxisto/lightsql/driver"
)

// open returns a database backed by an instance private to this test, and drops
// it afterwards so instances do not accumulate across a suite.
func open(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("lightsql", t.Name())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
		if !lightsqldriver.Drop(t.Name()) {
			t.Errorf("Drop(%q) found no instance", t.Name())
		}
	})
	return db
}

// TestEndToEnd is the acceptance test for the first milestone: a caller reaches
// the engine only through database/sql, and every stage of the pipeline runs.
func TestEndToEnd(t *testing.T) {
	db := open(t)

	if _, err := db.Exec(`CREATE TABLE users (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL, age INT)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	res, err := db.Exec(`INSERT INTO users (name, age) VALUES ('Alice', 30), ('Bob', 25)`)
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 2 {
		t.Errorf("RowsAffected = %d, %v; want 2, nil", n, err)
	}

	rows, err := db.Query(`SELECT id, name, age FROM users WHERE age > $1`, 26)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var id, age int64
		var name string
		if err := rows.Scan(&id, &name, &age); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, name)
		if id != 1 {
			t.Errorf("id = %d, want 1 (serial should start at 1)", id)
		}
		if age != 30 {
			t.Errorf("age = %d, want 30", age)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(got) != 1 || got[0] != "Alice" {
		t.Errorf("got %v, want [Alice]", got)
	}
}

// TestConnectionsShareData pins the property the whole test-fixture use case
// depends on: database/sql pools connections, so two statements may run on
// different ones and must still see the same data.
func TestConnectionsShareData(t *testing.T) {
	db := open(t)
	db.SetMaxOpenConns(4)

	if _, err := db.Exec(`CREATE TABLE t (a INT)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	ctx := context.Background()
	c1, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer c1.Close()
	c2, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer c2.Close()

	if _, err := c1.ExecContext(ctx, `INSERT INTO t (a) VALUES (7)`); err != nil {
		t.Fatalf("INSERT on c1: %v", err)
	}
	var a int
	if err := c2.QueryRowContext(ctx, `SELECT a FROM t`).Scan(&a); err != nil {
		t.Fatalf("SELECT on c2: %v", err)
	}
	if a != 7 {
		t.Errorf("a = %d, want 7", a)
	}
}

// TestSeparateInstancesAreIsolated is the other half of that contract: a
// different data source name is a different database.
func TestSeparateInstancesAreIsolated(t *testing.T) {
	a := open(t)
	b, err := sql.Open("lightsql", t.Name()+"-other")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() {
		b.Close()
		lightsqldriver.Drop(t.Name() + "-other")
	}()

	if _, err := a.Exec(`CREATE TABLE t (a INT)`); err != nil {
		t.Fatalf("CREATE TABLE on a: %v", err)
	}
	if _, err := b.Query(`SELECT a FROM t`); err == nil {
		t.Error("table created in one instance is visible in another")
	}
}

func TestBatchAndPrepared(t *testing.T) {
	db := open(t)

	// A fixture is usually one multi-statement Exec.
	_, err := db.Exec(`
		CREATE TABLE t (id INT, name TEXT);
		INSERT INTO t (id, name) VALUES (1, 'one');
		INSERT INTO t (id, name) VALUES (2, 'two');
	`)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}

	stmt, err := db.Prepare(`SELECT name FROM t WHERE id = $1`)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer stmt.Close()

	// Re-executing a prepared statement must give the same answer every time,
	// which fails if anything rewrites the plan while running it.
	for range 3 {
		for _, tc := range []struct{ id, want string }{{"1", "one"}, {"2", "two"}} {
			var name string
			if err := stmt.QueryRow(tc.id).Scan(&name); err != nil {
				t.Fatalf("QueryRow(%s): %v", tc.id, err)
			}
			if name != tc.want {
				t.Errorf("id %s: name = %q, want %q", tc.id, name, tc.want)
			}
		}
	}
}

// TestNullSemantics checks that SQL's three-valued logic survives the whole
// pipeline, not just the types package.
func TestNullSemantics(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (a INT, b TEXT);
		INSERT INTO t (a, b) VALUES (1, 'x'), (NULL, 'y');
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		name  string
		query string
		want  int
	}{
		// A comparison against NULL is unknown, and unknown does not pass a
		// filter — so this must not match the NULL row.
		{"equals null matches nothing", `SELECT a FROM t WHERE a = NULL`, 0},
		{"not equals null matches nothing", `SELECT a FROM t WHERE a <> NULL`, 0},
		{"is null matches the null row", `SELECT a FROM t WHERE a IS NULL`, 1},
		{"is not null matches the other", `SELECT a FROM t WHERE a IS NOT NULL`, 1},
		// NOT UNKNOWN is UNKNOWN, so negating a comparison against a NULL
		// column still does not match that row.
		{"not of unknown is unknown", `SELECT a FROM t WHERE NOT (a = 1)`, 0},
		{"is distinct from is definite", `SELECT a FROM t WHERE a IS DISTINCT FROM 1`, 1},
		// FALSE dominates AND even when the other side is unknown.
		{"false and unknown is false", `SELECT a FROM t WHERE 1 = 2 AND a = 1`, 0},
		// TRUE dominates OR even when the other side is unknown.
		{"true or unknown is true", `SELECT a FROM t WHERE 1 = 1 OR a = 1`, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := db.Query(tt.query)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()

			n := 0
			for rows.Next() {
				n++
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if n != tt.want {
				t.Errorf("%s returned %d rows, want %d", tt.query, n, tt.want)
			}
		})
	}
}

func TestNullScansIntoNullable(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (a INT);
		INSERT INTO t (a) VALUES (NULL);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var a sql.NullInt64
	if err := db.QueryRow(`SELECT a FROM t`).Scan(&a); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if a.Valid {
		t.Errorf("scanned %v, want NULL", a)
	}
}

func TestExpressions(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (a INT, b INT);
		INSERT INTO t (a, b) VALUES (2, 3);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		expr string
		want string
	}{
		// Precedence has to survive into execution, not just parsing.
		{"a * b + 1", "7"},
		{"a + b * 2", "8"},
		{"(a + b) * 2", "10"},
		{"a - b - 1", "-2"},
		{"b % a", "1"},
		{"a = 2 AND b = 3", "true"},
		{"a > b OR b > a", "true"},
		{"'x' || 'y'", "xy"},
		{"-a", "-2"},
		{"NOT (a = 2)", "false"},
	}

	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			var got string
			if err := db.QueryRow(`SELECT ` + tt.expr + ` FROM t`).Scan(&got); err != nil {
				t.Fatalf("SELECT %s: %v", tt.expr, err)
			}
			if got != tt.want {
				t.Errorf("SELECT %s = %q, want %q", tt.expr, got, tt.want)
			}
		})
	}
}

func TestSelectWithoutFrom(t *testing.T) {
	db := open(t)

	rows, err := db.Query(`SELECT 1 + 1`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()

	// Counting the rows rather than using QueryRow is deliberate: a SELECT with
	// no FROM must produce exactly one row, and an operator that forgets to
	// record that it already yielded it produces them forever. QueryRow reads
	// only the first row and would not notice.
	n := 0
	var got int
	for rows.Next() {
		n++
		if n > 1 {
			t.Fatal("SELECT without FROM produced more than one row")
		}
		if err := rows.Scan(&got); err != nil {
			t.Fatalf("Scan: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if n != 1 || got != 2 {
		t.Errorf("got %d rows with value %d, want 1 row with value 2", n, got)
	}
}

func TestLimitOffset(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (a INT);
		INSERT INTO t (a) VALUES (1), (2), (3), (4), (5);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		clause string
		want   []int
	}{
		{"LIMIT 2", []int{1, 2}},
		{"OFFSET 3", []int{4, 5}},
		{"LIMIT 2 OFFSET 1", []int{2, 3}},
		{"LIMIT 0", nil},
		{"LIMIT $1", []int{1}},
	}

	for _, tt := range tests {
		t.Run(tt.clause, func(t *testing.T) {
			var args []any
			if tt.clause == "LIMIT $1" {
				args = append(args, 1)
			}
			rows, err := db.Query(`SELECT a FROM t `+tt.clause, args...)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()

			var got []int
			for rows.Next() {
				var a int
				if err := rows.Scan(&a); err != nil {
					t.Fatalf("Scan: %v", err)
				}
				got = append(got, a)
			}
			if !equalInts(got, tt.want) {
				t.Errorf("%s gave %v, want %v", tt.clause, got, tt.want)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// queryInts runs a query and collects a single integer column, so the CRUD tests
// can state their expectations as a slice.
func queryInts(t *testing.T, db *sql.DB, query string) []int {
	t.Helper()

	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()

	var got []int
	for rows.Next() {
		var n sql.NullInt64
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, int(n.Int64))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return got
}

func TestUpdate(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (id INT, a INT, b INT);
		INSERT INTO t (id, a, b) VALUES (1, 10, 100), (2, 20, 200), (3, 30, 300);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	res, err := db.Exec(`UPDATE t SET a = 99 WHERE id = 2`)
	if err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("RowsAffected = %d, want 1", n)
	}
	// Ordered explicitly: an update rewrites the row as a new version, so the
	// updated row moves to the end of an unordered scan. SQL promises no order
	// without ORDER BY, and now that lightsql has it there is no reason for a
	// test to depend on the physical one.
	if got := queryInts(t, db, `SELECT a FROM t ORDER BY id`); !equalInts(got, []int{10, 99, 30}) {
		t.Errorf("after update, a = %v, want [10 99 30]", got)
	}

	// The right-hand side reads the row being updated.
	if _, err := db.Exec(`UPDATE t SET a = a + 1`); err != nil {
		t.Fatalf("UPDATE with self-reference: %v", err)
	}
	if got := queryInts(t, db, `SELECT a FROM t ORDER BY id`); !equalInts(got, []int{11, 100, 31}) {
		t.Errorf("after increment, a = %v, want [11 100 31]", got)
	}

	// Every assignment sees the original row, so this is a swap rather than two
	// copies of the same value. Applying assignments left to right would give
	// both columns the value of b.
	if _, err := db.Exec(`UPDATE t SET a = b, b = a WHERE id = 1`); err != nil {
		t.Fatalf("UPDATE swap: %v", err)
	}
	var a, b int
	if err := db.QueryRow(`SELECT a, b FROM t WHERE id = 1`).Scan(&a, &b); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if a != 100 || b != 11 {
		t.Errorf("after swap, a=%d b=%d; want a=100 b=11", a, b)
	}

	// A WHERE that matches nothing is not an error.
	res, err = db.Exec(`UPDATE t SET a = 0 WHERE id = 999`)
	if err != nil {
		t.Fatalf("UPDATE matching nothing: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Errorf("RowsAffected = %d, want 0", n)
	}
}

func TestDelete(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (id INT, a INT);
		INSERT INTO t (id, a) VALUES (1, 10), (2, 20), (3, 30), (4, 40);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	res, err := db.Exec(`DELETE FROM t WHERE a > 25`)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Errorf("RowsAffected = %d, want 2", n)
	}
	// Deleting must not disturb the order of the rows that remain.
	if got := queryInts(t, db, `SELECT id FROM t`); !equalInts(got, []int{1, 2}) {
		t.Errorf("remaining ids = %v, want [1 2]", got)
	}

	// A comparison against NULL is unknown, so a NULL row is not deleted by an
	// ordinary predicate.
	if _, err := db.Exec(`INSERT INTO t (id, a) VALUES (5, NULL)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM t WHERE a > 0`); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if got := queryInts(t, db, `SELECT id FROM t`); !equalInts(got, []int{5}) {
		t.Errorf("ids after deleting a > 0 = %v, want [5] (the NULL row survives)", got)
	}

	// DELETE without WHERE empties the table.
	if _, err := db.Exec(`DELETE FROM t`); err != nil {
		t.Fatalf("DELETE all: %v", err)
	}
	if got := queryInts(t, db, `SELECT id FROM t`); len(got) != 0 {
		t.Errorf("table not empty after unqualified DELETE: %v", got)
	}
}

func TestReturning(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (id BIGSERIAL PRIMARY KEY, a INT)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// The generated serial is visible to RETURNING; this is the portable
	// replacement for LastInsertId.
	var id int64
	if err := db.QueryRow(`INSERT INTO t (a) VALUES (7) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("INSERT RETURNING: %v", err)
	}
	if id != 1 {
		t.Errorf("returned id = %d, want 1", id)
	}

	// A multi-row INSERT returns one row per inserted row.
	got := queryInts(t, db, `INSERT INTO t (a) VALUES (8), (9) RETURNING id`)
	if !equalInts(got, []int{2, 3}) {
		t.Errorf("returned ids = %v, want [2 3]", got)
	}

	// UPDATE ... RETURNING reports the new value, not the old one.
	got = queryInts(t, db, `UPDATE t SET a = a * 10 WHERE a >= 8 RETURNING a`)
	if !equalInts(got, []int{80, 90}) {
		t.Errorf("UPDATE returned %v, want [80 90] (the new values)", got)
	}

	// DELETE ... RETURNING reports the row as it was before removal.
	got = queryInts(t, db, `DELETE FROM t WHERE a = 80 RETURNING a`)
	if !equalInts(got, []int{80}) {
		t.Errorf("DELETE returned %v, want [80]", got)
	}
	if got := queryInts(t, db, `SELECT a FROM t`); !equalInts(got, []int{7, 90}) {
		t.Errorf("after delete, a = %v, want [7 90]", got)
	}

	// RETURNING * expands to every column, and an alias renames the output.
	rows, err := db.Query(`INSERT INTO t (a) VALUES (5) RETURNING *`)
	if err != nil {
		t.Fatalf("RETURNING *: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	if len(cols) != 2 || cols[0] != "id" || cols[1] != "a" {
		t.Errorf("RETURNING * columns = %v, want [id a]", cols)
	}
}

// TestExecDiscardsReturning checks that a RETURNING statement run through Exec
// still applies, and reports its count rather than failing for having rows.
func TestExecDiscardsReturning(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (a INT)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	res, err := db.Exec(`INSERT INTO t (a) VALUES (1), (2) RETURNING a`)
	if err != nil {
		t.Fatalf("Exec with RETURNING: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Errorf("RowsAffected = %d, want 2", n)
	}
	if got := queryInts(t, db, `SELECT a FROM t`); !equalInts(got, []int{1, 2}) {
		t.Errorf("rows = %v, want [1 2]; the insert must still have happened", got)
	}
}

// TestUpdateFailureLeavesTableUnchanged pins that a statement failing partway
// does not leave half its work applied. Real atomicity arrives with MVCC; until
// then the mutation is staged and swapped in, which is what makes this hold.
func TestUpdateFailureLeavesTableUnchanged(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (id INT, a INT NOT NULL);
		INSERT INTO t (id, a) VALUES (1, 10), (2, 20);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// The second row violates NOT NULL, so the whole statement must fail.
	if _, err := db.Exec(`UPDATE t SET a = NULL WHERE id = 2`); err == nil {
		t.Fatal("UPDATE to NULL on a NOT NULL column succeeded, want an error")
	}
	if got := queryInts(t, db, `SELECT a FROM t`); !equalInts(got, []int{10, 20}) {
		t.Errorf("after the failed update, a = %v, want [10 20] unchanged", got)
	}
}

// queryStrings collects a single column as strings, so a test can state an
// expected ordering including NULLs.
func queryStrings(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()

	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if !v.Valid {
			got = append(got, "NULL")
			continue
		}
		got = append(got, v.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return got
}

func TestOrderBy(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (a INT, b INT, s TEXT);
		INSERT INTO t (a, b, s) VALUES
			(3, 1, 'c'), (1, 2, 'a'), (2, 1, 'b'), (1, 1, 'a');
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		name  string
		query string
		want  []int
	}{
		{"ascending is the default", `SELECT a FROM t ORDER BY a`, []int{1, 1, 2, 3}},
		{"explicit asc", `SELECT a FROM t ORDER BY a ASC`, []int{1, 1, 2, 3}},
		{"descending", `SELECT a FROM t ORDER BY a DESC`, []int{3, 2, 1, 1}},
		// The second key only decides rows that tie on the first.
		{"two keys", `SELECT b FROM t ORDER BY a, b DESC`, []int{2, 1, 1, 1}},
		// An expression is a legal sort term.
		{"by expression", `SELECT a FROM t ORDER BY a * -1`, []int{3, 2, 1, 1}},
		// A position refers to the select list.
		{"by position", `SELECT a FROM t ORDER BY 1 DESC`, []int{3, 2, 1, 1}},
		// A bare integer is a position, but an expression that merely contains
		// one is not: this sorts every row by the same constant, leaving the
		// input order rather than sorting by column 2.
		{"an arithmetic term is not a position", `SELECT a FROM t ORDER BY 1 + 1`, []int{3, 1, 2, 1}},
		// An output alias wins over anything else of that name.
		{"by alias", `SELECT a AS z FROM t ORDER BY z DESC`, []int{3, 2, 1, 1}},
		// A sort column need not appear in the select list at all.
		{"by an unselected column", `SELECT b FROM t ORDER BY s DESC, b`, []int{1, 1, 1, 2}},
		// ORDER BY runs before LIMIT, or the wrong rows survive.
		{"with limit", `SELECT a FROM t ORDER BY a DESC LIMIT 2`, []int{3, 2}},
		{"with offset", `SELECT a FROM t ORDER BY a LIMIT 2 OFFSET 2`, []int{2, 3}},
		// The filter runs before the sort.
		{"with where", `SELECT a FROM t WHERE a > 1 ORDER BY a DESC`, []int{3, 2}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := queryInts(t, db, tt.query); !equalInts(got, tt.want) {
				t.Errorf("%s\n got: %v\nwant: %v", tt.query, got, tt.want)
			}
		})
	}
}

// TestOrderByNulls pins where NULLs land. PostgreSQL treats NULL as larger than
// every other value, so the default follows the direction — and an explicit
// NULLS clause overrides it independently of that direction, which is the part
// that is easy to get backwards.
func TestOrderByNulls(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (a INT);
		INSERT INTO t (a) VALUES (2), (NULL), (1);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"asc defaults to nulls last", `SELECT a FROM t ORDER BY a`, []string{"1", "2", "NULL"}},
		{"desc defaults to nulls first", `SELECT a FROM t ORDER BY a DESC`, []string{"NULL", "2", "1"}},
		{"asc nulls first", `SELECT a FROM t ORDER BY a ASC NULLS FIRST`, []string{"NULL", "1", "2"}},
		// The combination that a naive implementation gets wrong: reversing the
		// comparison would move the NULLs too.
		{"desc nulls last", `SELECT a FROM t ORDER BY a DESC NULLS LAST`, []string{"2", "1", "NULL"}},
		{"asc nulls last", `SELECT a FROM t ORDER BY a ASC NULLS LAST`, []string{"1", "2", "NULL"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := queryStrings(t, db, tt.query)
			if len(got) != len(tt.want) {
				t.Fatalf("%s returned %v, want %v", tt.query, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("%s\n got: %v\nwant: %v", tt.query, got, tt.want)
					break
				}
			}
		})
	}
}

// TestOrderByIsStable checks that rows tying on every key keep their input
// order, so a test asserting on output is not at the mercy of the sort.
func TestOrderByIsStable(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (k INT, seq INT);
		INSERT INTO t (k, seq) VALUES (1, 1), (1, 2), (1, 3), (1, 4), (1, 5);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got := queryInts(t, db, `SELECT seq FROM t ORDER BY k`); !equalInts(got, []int{1, 2, 3, 4, 5}) {
		t.Errorf("rows tying on the sort key were reordered: %v", got)
	}
}

// TestClausesWithoutFrom pins that a SELECT with no FROM still accepts the
// clauses SQL allows on it. A missing FROM is a single-row source, not the
// absence of one, so nothing downstream needs to special-case it.
func TestClausesWithoutFrom(t *testing.T) {
	db := open(t)

	tests := []struct {
		name  string
		query string
		want  []int
	}{
		{"order by a position", `SELECT 1 ORDER BY 1`, []int{1}},
		{"order by an expression", `SELECT 2 ORDER BY 1 DESC`, []int{2}},
		{"where true", `SELECT 3 WHERE 1 = 1`, []int{3}},
		// A false predicate returns no rows rather than being rejected.
		{"where false", `SELECT 4 WHERE 1 = 2`, nil},
		{"limit", `SELECT 5 LIMIT 1`, []int{5}},
		{"limit zero", `SELECT 6 LIMIT 0`, nil},
		{"everything at once", `SELECT 7 WHERE 1 = 1 ORDER BY 1 LIMIT 1`, []int{7}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := queryInts(t, db, tt.query); !equalInts(got, tt.want) {
				t.Errorf("%s\n got: %v\nwant: %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestOrderByErrors(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (a INT)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{"unknown column", `SELECT a FROM t ORDER BY nope`, "42703"},
		// 42P10, invalid_column_reference, not 42601: the statement parses,
		// it just names a column that is not there. This said 42601 until the
		// parity suite compared it against a real PostgreSQL.
		{"position out of range", `SELECT a FROM t ORDER BY 2`, "42P10"},
		{"position zero", `SELECT a FROM t ORDER BY 0`, "42P10"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Query(tt.query)
			if err == nil {
				t.Fatalf("%s succeeded, want an error", tt.query)
			}
			var coded interface{ SQLState() string }
			if !errors.As(err, &coded) {
				t.Fatalf("error %v does not expose SQLState", err)
			}
			if got := coded.SQLState(); got != tt.want {
				t.Errorf("SQLSTATE = %s, want %s (error: %v)", got, tt.want, err)
			}
		})
	}
}

// sqlstate returns the SQLSTATE an error carries, or "" if it carries none.
func sqlstate(err error) string {
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		return coded.SQLState()
	}
	return ""
}

// TestUniqueConstraint is the regression test for constraints that parsed and
// were then silently ignored. A test asserting that a duplicate is rejected used
// to pass while the duplicate was happily inserted, which is worse than the
// feature being missing.
func TestUniqueConstraint(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE users (id INT PRIMARY KEY, email TEXT UNIQUE, name TEXT);
		INSERT INTO users (id, email, name) VALUES (1, 'a@x', 'Alice');
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		name  string
		query string
	}{
		{"duplicate primary key", `INSERT INTO users (id, email) VALUES (1, 'b@x')`},
		{"duplicate unique column", `INSERT INTO users (id, email) VALUES (2, 'a@x')`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Exec(tt.query)
			if err == nil {
				t.Fatalf("%s was accepted, want a unique violation", tt.query)
			}
			if got := sqlstate(err); got != "23505" {
				t.Errorf("SQLSTATE = %q, want 23505 (error: %v)", got, err)
			}
		})
	}

	// The rejected rows must not have landed.
	if got := queryInts(t, db, `SELECT id FROM users`); !equalInts(got, []int{1}) {
		t.Errorf("ids = %v, want [1]; a rejected insert was applied", got)
	}

	// A distinct value is still accepted.
	if _, err := db.Exec(`INSERT INTO users (id, email) VALUES (2, 'b@x')`); err != nil {
		t.Fatalf("distinct row rejected: %v", err)
	}
}

// TestUniqueAllowsMultipleNulls pins the rule that surprises people most: a NULL
// is never equal to anything, including another NULL, so UNIQUE permits any
// number of them. Using the grouping form of equality here — the one GROUP BY
// needs, where two NULLs are the same — would wrongly reject the second row.
func TestUniqueAllowsMultipleNulls(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (id INT PRIMARY KEY, code TEXT UNIQUE)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	for i := 1; i <= 3; i++ {
		if _, err := db.Exec(`INSERT INTO t (id, code) VALUES ($1, NULL)`, i); err != nil {
			t.Fatalf("row %d with a NULL unique column was rejected: %v", i, err)
		}
	}
	if got := queryInts(t, db, `SELECT id FROM t`); !equalInts(got, []int{1, 2, 3}) {
		t.Errorf("ids = %v, want [1 2 3]", got)
	}

	// A primary key column cannot be NULL at all, which is a different rule.
	if _, err := db.Exec(`INSERT INTO t (id, code) VALUES (NULL, 'x')`); sqlstate(err) != "23502" {
		t.Errorf("NULL primary key gave %v, want a not-null violation", err)
	}
}

// TestCompositeKeyChecksTheCombination is why constraints are modelled as a list
// of columns rather than a flag per column. PRIMARY KEY (a, b) requires the pair
// to be unique; treating it as "a is unique and b is unique" would reject rows
// that SQL permits.
func TestCompositeKeyChecksTheCombination(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (a INT, b INT, note TEXT, PRIMARY KEY (a, b));
		INSERT INTO t (a, b) VALUES (1, 1);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Repeating either column alone is fine; only the pair must be unique.
	for _, q := range []string{
		`INSERT INTO t (a, b) VALUES (1, 2)`,
		`INSERT INTO t (a, b) VALUES (2, 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s was rejected, but only the combination must be unique: %v", q, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO t (a, b) VALUES (1, 1)`); sqlstate(err) != "23505" {
		t.Errorf("duplicate pair gave %v, want a unique violation", err)
	}
}

// TestCompositeUniqueWithNulls covers the intersection of the two rules that are
// individually easy to get wrong: a UNIQUE constraint over several columns where
// one of them is NULL.
//
// A composite primary key cannot exercise this, because its columns are NOT
// NULL, and a single-column UNIQUE does not exercise the composite path. The
// combination is the case that a check written for either rule alone gets wrong.
func TestCompositeUniqueWithNulls(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (id INT PRIMARY KEY, a INT, b INT, UNIQUE (a, b));
		INSERT INTO t (id, a, b) VALUES (1, 1, 1);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Only the pair must be unique, so repeating either column alone is fine.
	for _, q := range []string{
		`INSERT INTO t (id, a, b) VALUES (2, 1, 2)`,
		`INSERT INTO t (id, a, b) VALUES (3, 2, 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s was rejected, but only the combination must be unique: %v", q, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO t (id, a, b) VALUES (4, 1, 1)`); sqlstate(err) != "23505" {
		t.Errorf("duplicate pair gave %v, want a unique violation", err)
	}

	// A NULL in any key column takes the row out of the check entirely, so
	// these rows do not conflict with each other even though their non-NULL
	// parts are identical. Comparing only the non-NULL columns, or treating
	// two NULLs as equal, would reject the second.
	for _, q := range []string{
		`INSERT INTO t (id, a, b) VALUES (5, 9, NULL)`,
		`INSERT INTO t (id, a, b) VALUES (6, 9, NULL)`,
		`INSERT INTO t (id, a, b) VALUES (7, NULL, 9)`,
		`INSERT INTO t (id, a, b) VALUES (8, NULL, NULL)`,
		`INSERT INTO t (id, a, b) VALUES (9, NULL, NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s was rejected; a NULL key column cannot conflict: %v", q, err)
		}
	}

	// Filling in the NULL is checked again, and must then conflict.
	if _, err := db.Exec(`UPDATE t SET b = 2 WHERE id = 5`); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if _, err := db.Exec(`UPDATE t SET a = 9, b = 2 WHERE id = 6`); sqlstate(err) != "23505" {
		t.Errorf("filling a NULL into a colliding pair gave %v, want a unique violation", err)
	}
}

// TestUniqueOnUpdate covers the cases a naive check gets wrong: a row must not
// conflict with itself, and a statement may pass through colliding intermediate
// states as long as the result is valid.
func TestUniqueOnUpdate(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (id INT PRIMARY KEY, rank INT UNIQUE);
		INSERT INTO t (id, rank) VALUES (1, 10), (2, 20), (3, 30);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Rewriting a row with its own value is not a conflict with itself.
	if _, err := db.Exec(`UPDATE t SET rank = rank WHERE id = 1`); err != nil {
		t.Errorf("updating a row to its own value was rejected: %v", err)
	}

	// Every row shifts by one. Checking row by row would see 20 collide with
	// the not-yet-updated row 2, but the final set is unique.
	if _, err := db.Exec(`UPDATE t SET rank = rank + 10`); err != nil {
		t.Errorf("a shift that only collides mid-statement was rejected: %v", err)
	}
	if got := queryInts(t, db, `SELECT rank FROM t ORDER BY rank`); !equalInts(got, []int{20, 30, 40}) {
		t.Errorf("ranks = %v, want [20 30 40]", got)
	}

	// A genuine collision is still refused, and leaves the table unchanged.
	if _, err := db.Exec(`UPDATE t SET rank = 20 WHERE id = 3`); sqlstate(err) != "23505" {
		t.Errorf("colliding update gave %v, want a unique violation", err)
	}
	if got := queryInts(t, db, `SELECT rank FROM t ORDER BY rank`); !equalInts(got, []int{20, 30, 40}) {
		t.Errorf("ranks = %v after a refused update, want [20 30 40] unchanged", got)
	}

	// Deleting a row frees its value for another.
	if _, err := db.Exec(`DELETE FROM t WHERE id = 1`); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if _, err := db.Exec(`UPDATE t SET rank = 20 WHERE id = 3`); err != nil {
		t.Errorf("reusing a deleted row's value was rejected: %v", err)
	}
}

// TestUniqueViolationNamesTheConstraint checks the message, since application
// code and humans both use it to tell which constraint failed.
func TestUniqueViolationNamesTheConstraint(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE users (id INT PRIMARY KEY, email TEXT UNIQUE);
		INSERT INTO users (id, email) VALUES (1, 'a@x');
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		query string
		want  string
	}{
		// PostgreSQL derives these names, and application code sometimes
		// matches on them.
		{`INSERT INTO users (id, email) VALUES (1, 'b@x')`, "users_pkey"},
		{`INSERT INTO users (id, email) VALUES (2, 'a@x')`, "users_email_key"},
	}
	for _, tt := range tests {
		_, err := db.Exec(tt.query)
		if err == nil {
			t.Fatalf("%s was accepted", tt.query)
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("error %q does not name the constraint %q", err, tt.want)
		}
	}

	// An explicitly named constraint is reported under that name.
	if _, err := db.Exec(`
		CREATE TABLE c (a INT, CONSTRAINT my_key UNIQUE (a));
		INSERT INTO c (a) VALUES (1);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := db.Exec(`INSERT INTO c (a) VALUES (1)`)
	if err == nil || !strings.Contains(err.Error(), "my_key") {
		t.Errorf("error %v does not name the constraint my_key", err)
	}
}

func TestColumnDefaults(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (
			id     INT PRIMARY KEY,
			n      INT  DEFAULT 42,
			s      TEXT DEFAULT 'pending',
			flag   BOOLEAN DEFAULT true,
			calc   INT  DEFAULT 6 * 7,
			maybe  INT  DEFAULT NULL,
			plain  INT
		);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// An omitted column takes its default.
	if _, err := db.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	var (
		n, calc      int
		s            string
		flag         bool
		maybe, plain sql.NullInt64
	)
	if err := db.QueryRow(`SELECT n, s, flag, calc, maybe, plain FROM t WHERE id = 1`).
		Scan(&n, &s, &flag, &calc, &maybe, &plain); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	switch {
	case n != 42:
		t.Errorf("n = %d, want 42", n)
	case s != "pending":
		t.Errorf("s = %q, want \"pending\"", s)
	case !flag:
		t.Error("flag = false, want true")
	case calc != 42:
		t.Errorf("calc = %d, want 42 from the expression 6 * 7", calc)
	case maybe.Valid:
		t.Errorf("maybe = %v, want NULL from DEFAULT NULL", maybe)
	case plain.Valid:
		t.Errorf("plain = %v, want NULL for a column with no default", plain)
	}

	// An explicit value wins over the default, including an explicit NULL.
	if _, err := db.Exec(`INSERT INTO t (id, n, s) VALUES (2, 7, NULL)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	var n2 int
	var s2 sql.NullString
	if err := db.QueryRow(`SELECT n, s FROM t WHERE id = 2`).Scan(&n2, &s2); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if n2 != 7 {
		t.Errorf("n = %d, want the explicit 7 rather than the default", n2)
	}
	if s2.Valid {
		t.Errorf("s = %v, want an explicit NULL rather than the default", s2)
	}

	// SERIAL is itself a default, so a table with both must not have the two
	// fight over the same omitted column.
	if _, err := db.Exec(`
		CREATE TABLE u (id BIGSERIAL PRIMARY KEY, n INT DEFAULT 5);
		INSERT INTO u (n) VALUES (1);
		INSERT INTO u (n) VALUES (2);
	`); err != nil {
		t.Fatalf("serial with a defaulted column: %v", err)
	}
	if got := queryInts(t, db, `SELECT id FROM u ORDER BY id`); !equalInts(got, []int{1, 2}) {
		t.Errorf("serial ids = %v, want [1 2]", got)
	}
	// Omitting both takes the sequence for id and the default for n.
	if _, err := db.Exec(`INSERT INTO u (id) VALUES (99)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if got := queryInts(t, db, `SELECT n FROM u WHERE id = 99`); !equalInts(got, []int{5}) {
		t.Errorf("n = %v for an omitted defaulted column, want [5]", got)
	}
}

// TestCheckConstraint pins the rule that is the exact inverse of a WHERE clause:
// a CHECK is satisfied by true *or unknown*, and violated only by false.
func TestCheckConstraint(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (
			id  INT PRIMARY KEY,
			n   INT CHECK (n >= 0),
			lo  INT,
			hi  INT,
			CONSTRAINT lo_below_hi CHECK (lo < hi)
		);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO t (id, n, lo, hi) VALUES (1, 5, 1, 2)`); err != nil {
		t.Fatalf("a satisfying row was rejected: %v", err)
	}

	tests := []struct {
		name  string
		query string
	}{
		{"column check", `INSERT INTO t (id, n, lo, hi) VALUES (2, -1, 1, 2)`},
		{"table check", `INSERT INTO t (id, n, lo, hi) VALUES (3, 1, 9, 2)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Exec(tt.query)
			if err == nil {
				t.Fatalf("%s was accepted, want a check violation", tt.query)
			}
			if got := sqlstate(err); got != "23514" {
				t.Errorf("SQLSTATE = %q, want 23514 (error: %v)", got, err)
			}
		})
	}

	// A NULL makes the predicate unknown, and unknown satisfies a CHECK. Using
	// the filter rule — keep only true — would turn every CHECK into an
	// implicit NOT NULL, which is the mistake this pins.
	if _, err := db.Exec(`INSERT INTO t (id, n, lo, hi) VALUES (4, NULL, NULL, 2)`); err != nil {
		t.Errorf("a row whose check is unknown was rejected: %v", err)
	}

	// Updates are checked too, and a violation leaves the table unchanged.
	if _, err := db.Exec(`UPDATE t SET n = -1 WHERE id = 1`); sqlstate(err) != "23514" {
		t.Errorf("violating update gave %v, want a check violation", err)
	}
	var n int
	if err := db.QueryRow(`SELECT n FROM t WHERE id = 1`).Scan(&n); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if n != 5 {
		t.Errorf("n = %d after a refused update, want 5 unchanged", n)
	}
	if _, err := db.Exec(`UPDATE t SET n = 10 WHERE id = 1`); err != nil {
		t.Errorf("a satisfying update was rejected: %v", err)
	}

	// The constraint is named in the error, using the written name where there
	// is one.
	_, err := db.Exec(`INSERT INTO t (id, lo, hi) VALUES (5, 9, 2)`)
	if err == nil || !strings.Contains(err.Error(), "lo_below_hi") {
		t.Errorf("error %v does not name the constraint lo_below_hi", err)
	}
}

// TestUnnamedChecksGetDistinctNames pins that two unnamed CHECKs on one table
// are named apart, so a violation says which one failed. Naming both after the
// table would attribute either failure to the same constraint.
func TestUnnamedChecksGetDistinctNames(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (a INT CHECK (a > 0), b INT CHECK (b > 0), c INT CHECK (c > 0));
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// PostgreSQL numbers them t_check, t_check1, t_check2.
	tests := []struct {
		query string
		want  string
	}{
		{`INSERT INTO t (a, b, c) VALUES (-1, 1, 1)`, "t_check"},
		{`INSERT INTO t (a, b, c) VALUES (1, -1, 1)`, "t_check1"},
		{`INSERT INTO t (a, b, c) VALUES (1, 1, -1)`, "t_check2"},
	}
	for _, tt := range tests {
		_, err := db.Exec(tt.query)
		if err == nil {
			t.Fatalf("%s was accepted", tt.query)
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("error %q does not name %q", err, tt.want)
		}
	}

	// A derived name must not collide with one written explicitly.
	if _, err := db.Exec(`
		CREATE TABLE u (a INT CONSTRAINT u_check CHECK (a > 0), b INT CHECK (b > 0));
		INSERT INTO u (a, b) VALUES (1, -1);
	`); err == nil {
		t.Fatal("violating row was accepted")
	} else if !strings.Contains(err.Error(), "u_check1") {
		t.Errorf("error %q should name u_check1, since u_check was taken explicitly", err)
	}
}

// TestConstraintsRejectedAtCreate checks that a bad DEFAULT or CHECK fails when
// the table is created, not at the first insert — by which point the schema is
// already in place and the error is far from its cause.
func TestConstraintsRejectedAtCreate(t *testing.T) {
	db := open(t)

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{"check names an unknown column", `CREATE TABLE a (n INT CHECK (nope > 0))`, "42703"},
		{"check is not boolean", `CREATE TABLE b (n INT CHECK (n + 1))`, "42804"},
		{"default references a column", `CREATE TABLE c (n INT, m INT DEFAULT n)`, "42703"},
		{"default has the wrong type", `CREATE TABLE d (n INT DEFAULT 'abc')`, "22P02"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Exec(tt.query)
			if err == nil {
				t.Fatalf("%s was accepted, want an error", tt.query)
			}
			if got := sqlstate(err); got != tt.want {
				t.Errorf("SQLSTATE = %q, want %q (error: %v)", got, tt.want, err)
			}
		})
	}
}

// TestTransactionCommitAndRollback is the reason M2 exists: the standard
// test-isolation idiom is Begin then a deferred Rollback, and it has to actually
// undo the work rather than merely be accepted.
func TestTransactionCommitAndRollback(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (id INT PRIMARY KEY, n INT)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Committed work sticks.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO t (id, n) VALUES (1, 10)`); err != nil {
		t.Fatalf("INSERT in tx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := queryInts(t, db, `SELECT n FROM t`); !equalInts(got, []int{10}) {
		t.Errorf("after commit, n = %v, want [10]", got)
	}

	// Rolled back work does not — including updates and deletes, which is
	// exactly what ramsql's undo log silently fails to reverse.
	tx, err = db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO t (id, n) VALUES (2, 20)`,
		`UPDATE t SET n = 999 WHERE id = 1`,
		`DELETE FROM t WHERE id = 1`,
	} {
		if _, err := tx.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := queryInts(t, db, `SELECT n FROM t ORDER BY id`); !equalInts(got, []int{10}) {
		t.Errorf("after rollback, n = %v, want [10] unchanged", got)
	}
}

// TestTransactionSeesItsOwnWrites checks that a transaction reads its own
// uncommitted changes while nobody else can.
func TestTransactionSeesItsOwnWrites(t *testing.T) {
	db := open(t)
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(`CREATE TABLE t (id INT PRIMARY KEY, n INT)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT INTO t (id, n) VALUES (1, 10)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	var n int
	if err := tx.QueryRow(`SELECT n FROM t WHERE id = 1`).Scan(&n); err != nil {
		t.Fatalf("a transaction cannot see its own insert: %v", err)
	}
	if n != 10 {
		t.Errorf("n = %d, want 10", n)
	}

	// Outside the transaction, on another connection, the row does not exist.
	if got := queryInts(t, db, `SELECT n FROM t`); len(got) != 0 {
		t.Errorf("uncommitted row visible outside the transaction: %v", got)
	}
}

// TestRepeatableReadKeepsItsSnapshot pins the difference between the two
// isolation levels, which is the thing sql.TxOptions asks for and which ramsql
// accepts and discards.
func TestRepeatableReadKeepsItsSnapshot(t *testing.T) {
	db := open(t)
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(`
		CREATE TABLE t (id INT PRIMARY KEY, n INT);
		INSERT INTO t (id, n) VALUES (1, 10);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ctx := context.Background()

	for _, tt := range []struct {
		name     string
		level    sql.IsolationLevel
		wantSame bool
	}{
		// One snapshot for the whole transaction, so the second read matches
		// the first even though the row changed in between.
		{"repeatable read", sql.LevelRepeatableRead, true},
		// A fresh snapshot per statement, so the second read sees the change.
		{"read committed", sql.LevelReadCommitted, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := db.Exec(`UPDATE t SET n = 10 WHERE id = 1`); err != nil {
				t.Fatalf("reset: %v", err)
			}

			tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: tt.level})
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			defer tx.Rollback()

			var first int
			if err := tx.QueryRow(`SELECT n FROM t WHERE id = 1`).Scan(&first); err != nil {
				t.Fatalf("first read: %v", err)
			}

			// Someone else commits a change while the transaction is open.
			if _, err := db.Exec(`UPDATE t SET n = 20 WHERE id = 1`); err != nil {
				t.Fatalf("concurrent update: %v", err)
			}

			var second int
			if err := tx.QueryRow(`SELECT n FROM t WHERE id = 1`).Scan(&second); err != nil {
				t.Fatalf("second read: %v", err)
			}

			if same := first == second; same != tt.wantSame {
				t.Errorf("%s: reads were %d then %d; same=%v, want same=%v",
					tt.name, first, second, same, tt.wantSame)
			}
		})
	}
}

// TestReadOnlyTransactionRefusesWrites checks that sql.TxOptions.ReadOnly is
// honoured rather than accepted and ignored.
func TestReadOnlyTransactionRefusesWrites(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (id INT PRIMARY KEY, n INT);
		INSERT INTO t (id, n) VALUES (1, 10);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	// Reading is fine.
	var n int
	if err := tx.QueryRow(`SELECT n FROM t`).Scan(&n); err != nil {
		t.Fatalf("read in a read-only transaction: %v", err)
	}
	// Writing is not.
	_, err = tx.Exec(`INSERT INTO t (id, n) VALUES (2, 20)`)
	if got := sqlstate(err); got != "25006" {
		t.Errorf("write in a read-only transaction gave %v (SQLSTATE %q), want 25006", err, got)
	}
}

// TestFailedStatementPoisonsTransaction pins PostgreSQL's rule that a statement
// error aborts the transaction: later commands are refused until the caller
// rolls back, rather than being allowed to build on a broken state.
func TestFailedStatementPoisonsTransaction(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (id INT PRIMARY KEY)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	// A duplicate key fails the statement.
	if _, err := tx.Exec(`INSERT INTO t (id) VALUES (1)`); sqlstate(err) != "23505" {
		t.Fatalf("duplicate insert gave %v, want a unique violation", err)
	}
	// Everything after it is refused until the transaction ends.
	if _, err := tx.Exec(`INSERT INTO t (id) VALUES (2)`); sqlstate(err) != "25P02" {
		t.Errorf("a statement after a failure gave %v, want 25P02", err)
	}
	// Committing a failed transaction rolls it back rather than keeping part.
	if err := tx.Commit(); err == nil {
		t.Error("committing a failed transaction succeeded")
	}
	if got := queryInts(t, db, `SELECT id FROM t`); len(got) != 0 {
		t.Errorf("a failed transaction left %v behind", got)
	}
}

// TestConnectionIsReusableAfterRollback checks that a connection returned to the
// pool after a rollback carries nothing over: the changes are gone and the
// connection still works.
//
// It deliberately does not test the abandoned-transaction path. database/sql
// will not return a connection to the pool while its Tx is live, so a test
// cannot reach ResetSession with a transaction still open through the public
// API; TestResetSessionRollsBackAnOpenTransaction drives the driver directly
// for that.
func TestConnectionIsReusableAfterRollback(t *testing.T) {
	db := open(t)
	db.SetMaxOpenConns(1) // force the same connection to be reused
	if _, err := db.Exec(`CREATE TABLE t (id INT PRIMARY KEY)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got := queryInts(t, db, `SELECT id FROM t`); len(got) != 0 {
		t.Errorf("the rolled back transaction left %v behind", got)
	}
	// The connection is still usable afterwards.
	if _, err := db.Exec(`INSERT INTO t (id) VALUES (2)`); err != nil {
		t.Errorf("the connection was not reusable: %v", err)
	}
}

// TestResetSessionRollsBackAnOpenTransaction drives the driver directly, because
// database/sql will not return a connection to the pool while its Tx is live —
// so the leak this guards against is unreachable through the public API.
//
// It is still worth guarding: the pool calls ResetSession before handing a
// connection on, and a transaction surviving that would give the next caller
// someone else's snapshot.
func TestResetSessionRollsBackAnOpenTransaction(t *testing.T) {
	ctx := context.Background()
	connector, err := lightsqldriver.NewConnector(t.Name())
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	t.Cleanup(func() { lightsqldriver.Drop(t.Name()) })

	conn, err := connector.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	execer := conn.(driver.ExecerContext)
	if _, err := execer.ExecContext(ctx, `CREATE TABLE t (id INT PRIMARY KEY)`, nil); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	// Open a transaction, write, and then abandon it without committing.
	if _, err := conn.(driver.ConnBeginTx).BeginTx(ctx, driver.TxOptions{}); err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := execer.ExecContext(ctx, `INSERT INTO t (id) VALUES (1)`, nil); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// The pool does this before handing the connection to the next caller.
	if err := conn.(driver.SessionResetter).ResetSession(ctx); err != nil {
		t.Fatalf("ResetSession: %v", err)
	}

	// The abandoned write is gone, and the connection is in autocommit again.
	rows, err := conn.(driver.QueryerContext).QueryContext(ctx, `SELECT id FROM t`, nil)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	defer rows.Close()
	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err == nil {
		t.Errorf("the abandoned transaction left row %v behind", dest[0])
	}
}

// TestReadOnlyRefusesDDL pins that a read-only transaction cannot reshape the
// catalog either. DDL is a write, and leaving it out of the check would let
// CREATE TABLE through while INSERT was refused.
func TestReadOnlyRefusesDDL(t *testing.T) {
	db := open(t)

	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`CREATE TABLE t (a INT)`); sqlstate(err) != "25006" {
		t.Errorf("CREATE TABLE in a read-only transaction gave %v, want SQLSTATE 25006", err)
	}
}

// fkFixture creates a parent and a child table whose reference uses the given
// referential actions.
func fkFixture(t *testing.T, db *sql.DB, actions string) {
	t.Helper()
	if _, err := db.Exec(`
		CREATE TABLE parent (id INT PRIMARY KEY, label TEXT);
		CREATE TABLE child (id INT PRIMARY KEY, pid INT REFERENCES parent (id) ` + actions + `);
		INSERT INTO parent (id, label) VALUES (1, 'a'), (2, 'b');
		INSERT INTO child (id, pid) VALUES (10, 1), (11, 1), (12, 2);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func TestForeignKeyRejectsMissingParent(t *testing.T) {
	db := open(t)
	fkFixture(t, db, "")

	// A reference to a row that does not exist is refused.
	_, err := db.Exec(`INSERT INTO child (id, pid) VALUES (99, 404)`)
	if got := sqlstate(err); got != "23503" {
		t.Errorf("insert with a missing parent gave %v (SQLSTATE %q), want 23503", err, got)
	}
	// So is an update that points a row at nothing.
	_, err = db.Exec(`UPDATE child SET pid = 404 WHERE id = 10`)
	if got := sqlstate(err); got != "23503" {
		t.Errorf("update to a missing parent gave %v (SQLSTATE %q), want 23503", err, got)
	}
	// An existing parent is fine.
	if _, err := db.Exec(`INSERT INTO child (id, pid) VALUES (13, 2)`); err != nil {
		t.Errorf("insert with a valid parent was rejected: %v", err)
	}
}

// TestForeignKeyNullIsUnconstrained pins SQL's MATCH SIMPLE default: a NULL in
// the key means the reference is not specified, so there is nothing for it to
// fail to match. Treating NULL as a value to look up would reject rows
// PostgreSQL accepts.
func TestForeignKeyNullIsUnconstrained(t *testing.T) {
	db := open(t)
	fkFixture(t, db, "")

	if _, err := db.Exec(`INSERT INTO child (id, pid) VALUES (20, NULL)`); err != nil {
		t.Errorf("a NULL reference was rejected: %v", err)
	}
	if _, err := db.Exec(`UPDATE child SET pid = NULL WHERE id = 10`); err != nil {
		t.Errorf("setting a reference to NULL was rejected: %v", err)
	}
}

func TestForeignKeyReferentialActions(t *testing.T) {
	tests := []struct {
		name      string
		actions   string
		deleteErr string // SQLSTATE expected from deleting parent 1, "" if allowed
		wantChild []int  // child ids remaining afterwards
		wantPid   string // pid of child 10 afterwards, "" if the row is gone
	}{
		{
			// The default refuses while references remain.
			name: "no action", actions: "", deleteErr: "23503",
			wantChild: []int{10, 11, 12}, wantPid: "1",
		},
		{
			name: "restrict", actions: "ON DELETE RESTRICT", deleteErr: "23503",
			wantChild: []int{10, 11, 12}, wantPid: "1",
		},
		{
			// The children go with the parent.
			name: "cascade", actions: "ON DELETE CASCADE",
			wantChild: []int{12},
		},
		{
			// The children stay, pointing at nothing.
			name: "set null", actions: "ON DELETE SET NULL",
			wantChild: []int{10, 11, 12}, wantPid: "NULL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := open(t)
			fkFixture(t, db, tt.actions)

			_, err := db.Exec(`DELETE FROM parent WHERE id = 1`)
			if got := sqlstate(err); got != tt.deleteErr {
				t.Fatalf("delete gave %v (SQLSTATE %q), want %q", err, got, tt.deleteErr)
			}

			if got := queryInts(t, db, `SELECT id FROM child ORDER BY id`); !equalInts(got, tt.wantChild) {
				t.Errorf("remaining child ids = %v, want %v", got, tt.wantChild)
			}
			if tt.wantPid != "" {
				got := queryStrings(t, db, `SELECT pid FROM child WHERE id = 10`)
				if len(got) != 1 || got[0] != tt.wantPid {
					t.Errorf("child 10 pid = %v, want [%s]", got, tt.wantPid)
				}
			}
		})
	}
}

// TestForeignKeyOnUpdateCascade checks the other half of the actions, and that
// an update which does not touch the referenced key is not a referential event
// at all.
func TestForeignKeyOnUpdateCascade(t *testing.T) {
	db := open(t)
	fkFixture(t, db, "ON UPDATE CASCADE")

	// Changing a column the children do not reference must not disturb them.
	if _, err := db.Exec(`UPDATE parent SET label = 'renamed' WHERE id = 1`); err != nil {
		t.Fatalf("UPDATE of a non-key column: %v", err)
	}
	if got := queryInts(t, db, `SELECT pid FROM child WHERE id = 10`); !equalInts(got, []int{1}) {
		t.Errorf("a non-key update disturbed the children: pid = %v, want [1]", got)
	}

	// Changing the key drags the children with it.
	if _, err := db.Exec(`UPDATE parent SET id = 100 WHERE id = 1`); err != nil {
		t.Fatalf("UPDATE of the key: %v", err)
	}
	if got := queryInts(t, db, `SELECT pid FROM child ORDER BY id`); !equalInts(got, []int{100, 100, 2}) {
		t.Errorf("after ON UPDATE CASCADE, pids = %v, want [100 100 2]", got)
	}
}

// TestForeignKeyRollsBackWithTheTransaction checks that a cascade is part of the
// transaction rather than a side effect that outlives it.
func TestForeignKeyRollsBackWithTheTransaction(t *testing.T) {
	db := open(t)
	fkFixture(t, db, "ON DELETE CASCADE")

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(`DELETE FROM parent WHERE id = 1`); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got := queryInts(t, db, `SELECT id FROM child ORDER BY id`); !equalInts(got, []int{10, 11, 12}) {
		t.Errorf("after rollback, child ids = %v, want [10 11 12]; the cascade outlived its transaction", got)
	}
	if got := queryInts(t, db, `SELECT id FROM parent ORDER BY id`); !equalInts(got, []int{1, 2}) {
		t.Errorf("after rollback, parent ids = %v, want [1 2]", got)
	}
}

// TestForeignKeySelfReference covers a table pointing at itself, which is how a
// tree is usually stored and which the binder has to allow even though the table
// is not in the catalog yet while it is being created.
func TestForeignKeySelfReference(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE node (id INT PRIMARY KEY, parent_id INT REFERENCES node (id) ON DELETE CASCADE);
		INSERT INTO node (id, parent_id) VALUES (1, NULL), (2, 1), (3, 1);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO node (id, parent_id) VALUES (4, 99)`); sqlstate(err) != "23503" {
		t.Errorf("a self-reference to a missing row gave %v, want 23503", err)
	}
	if _, err := db.Exec(`DELETE FROM node WHERE id = 1`); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if got := queryInts(t, db, `SELECT id FROM node`); len(got) != 0 {
		t.Errorf("after deleting the root, nodes = %v, want none", got)
	}
}

func TestForeignKeyDefinitionErrors(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE p (id INT PRIMARY KEY, plain INT)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{"unknown table", `CREATE TABLE a (x INT REFERENCES nope (id))`, "42P01"},
		{"unknown column", `CREATE TABLE b (x INT REFERENCES p (nope))`, "42703"},
		// A reference must point at something unique, or a cascade would have
		// no single row to follow.
		{"not unique", `CREATE TABLE c (x INT REFERENCES p (plain))`, "42704"},
		{"type mismatch", `CREATE TABLE d (x TEXT REFERENCES p (id))`, "42804"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Exec(tt.query)
			if got := sqlstate(err); got != tt.want {
				t.Errorf("%s gave %v (SQLSTATE %q), want %q", tt.query, err, got, tt.want)
			}
		})
	}
}

// TestErrorsCarrySQLState checks the contract application code relies on to tell
// one failure from another, using the same interface pgx and lib/pq expose.
func TestErrorsCarrySQLState(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (a INT NOT NULL)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{"syntax error", `SELCT 1`, "42601"},
		{"undefined table", `SELECT a FROM missing`, "42P01"},
		{"undefined column", `SELECT missing FROM t`, "42703"},
		{"duplicate table", `CREATE TABLE t (a INT)`, "42P07"},
		{"undefined type", `CREATE TABLE u (a NOSUCHTYPE)`, "42704"},
		{"not null violation", `INSERT INTO t (a) VALUES (NULL)`, "23502"},
		{"division by zero", `SELECT 1 / 0`, "22012"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Exec(tt.query)
			if err == nil {
				t.Fatalf("%s succeeded, want an error", tt.query)
			}
			var coded interface{ SQLState() string }
			if !errors.As(err, &coded) {
				t.Fatalf("error %v does not expose SQLState", err)
			}
			if got := coded.SQLState(); got != tt.want {
				t.Errorf("SQLSTATE = %s, want %s (error: %v)", got, tt.want, err)
			}
		})
	}
}

// TestColumnTypes covers the introspection ORMs perform before mapping a result
// set.
func TestColumnTypes(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (a BIGINT, b VARCHAR(255), c BOOLEAN, d TIMESTAMP WITH TIME ZONE)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	rows, err := db.Query(`SELECT a, b, c, d FROM t`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	cols, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("ColumnTypes: %v", err)
	}
	want := []string{"bigint", "character varying", "boolean", "timestamp with time zone"}
	if len(cols) != len(want) {
		t.Fatalf("got %d columns, want %d", len(cols), len(want))
	}
	for i, w := range want {
		if got := cols[i].DatabaseTypeName(); got != w {
			t.Errorf("column %d type name = %q, want %q", i, got, w)
		}
	}

	names, err := rows.Columns()
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	for i, w := range []string{"a", "b", "c", "d"} {
		if names[i] != w {
			t.Errorf("column %d name = %q, want %q", i, names[i], w)
		}
	}
}

// TestContextCancellation checks that cancellation reaches the operator tree,
// rather than only being tested when the statement starts.
func TestContextCancellation(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`
		CREATE TABLE t (a INT);
		INSERT INTO t (a) VALUES (1), (2), (3);
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := db.QueryContext(ctx, `SELECT a FROM t`); !errors.Is(err, context.Canceled) {
		t.Errorf("QueryContext with a cancelled context returned %v, want context.Canceled", err)
	}
}

func TestPingAndClose(t *testing.T) {
	db := open(t)
	if err := db.PingContext(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestArgumentTypes(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (i INT, f DOUBLE PRECISION, s TEXT, b BOOLEAN, y BYTEA, ts TIMESTAMP)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	when := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO t (i, f, s, b, y, ts) VALUES ($1, $2, $3, $4, $5, $6)`,
		int64(42), 1.5, "hello", true, []byte{1, 2, 3}, when); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	var (
		i  int64
		f  float64
		s  string
		bo bool
		y  []byte
		ts time.Time
	)
	if err := db.QueryRow(`SELECT i, f, s, b, y, ts FROM t`).Scan(&i, &f, &s, &bo, &y, &ts); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if i != 42 || f != 1.5 || s != "hello" || !bo || string(y) != "\x01\x02\x03" {
		t.Errorf("round trip gave %d %v %q %v %v", i, f, s, bo, y)
	}
	if !ts.Equal(when) {
		t.Errorf("timestamp = %v, want %v", ts, when)
	}
}
