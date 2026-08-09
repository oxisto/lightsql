package driver_test

import (
	"context"
	"database/sql"
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
	if got := queryInts(t, db, `SELECT a FROM t`); !equalInts(got, []int{10, 99, 30}) {
		t.Errorf("after update, a = %v, want [10 99 30]", got)
	}

	// The right-hand side reads the row being updated.
	if _, err := db.Exec(`UPDATE t SET a = a + 1`); err != nil {
		t.Fatalf("UPDATE with self-reference: %v", err)
	}
	if got := queryInts(t, db, `SELECT a FROM t`); !equalInts(got, []int{11, 100, 31}) {
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
		{"position out of range", `SELECT a FROM t ORDER BY 2`, "42601"},
		{"position zero", `SELECT a FROM t ORDER BY 0`, "42601"},
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
