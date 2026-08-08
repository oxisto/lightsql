package driver_test

import (
	"context"
	"database/sql"
	"errors"
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
