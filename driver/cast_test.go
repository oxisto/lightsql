package driver_test

import (
	"testing"
	"time"
)

// TestParameterTakesTheColumnType is the regression test for a parameter whose
// type was never inferred on the insert path.
//
// An unresolved parameter reports KindNull, and the binder's coercion treated
// that as "compatible with anything" and returned before recording the type the
// executor had to convert to. Comparisons were unaffected, because they resolve
// parameters separately — so the bug was invisible in a WHERE clause and
// corrupted the column on the way in, which is the worse half.
func TestParameterTakesTheColumnType(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (n INT, f DOUBLE PRECISION)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Both arguments are strings, which is what a caller reading from a config
	// file or a CSV actually has.
	if _, err := db.Exec(`INSERT INTO t VALUES ($1, $2)`, "7", "1.5"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// If the value had been stored as text, this predicate would match nothing.
	var n int
	if err := db.QueryRow(`SELECT n FROM t WHERE n = 7`).Scan(&n); err != nil {
		t.Fatalf("integer column did not store an integer: %v", err)
	}
	if n != 7 {
		t.Errorf("n = %d, want 7", n)
	}

	var f float64
	if err := db.QueryRow(`SELECT f FROM t WHERE f > 1`).Scan(&f); err != nil {
		t.Fatalf("float column did not store a float: %v", err)
	}
	if f != 1.5 {
		t.Errorf("f = %v, want 1.5", f)
	}

	// A NULL argument must still be accepted, since the fix must not turn
	// "type not yet known" into "must be the column type".
	if _, err := db.Exec(`INSERT INTO t VALUES ($1, $2)`, nil, nil); err != nil {
		t.Fatalf("NULL argument: %v", err)
	}
}

// TestTimestampParameterKinds covers the conversion the fix exposed: a
// time.Time argument arrives as timestamptz because the driver cannot tell
// which of the two a caller meant, so it has to reach a timestamp column.
func TestTimestampParameterKinds(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (ts TIMESTAMP, tz TIMESTAMPTZ, d DATE)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	when := time.Date(2026, 8, 8, 12, 30, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO t VALUES ($1, $1, $1)`, when); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var ts, tz, d time.Time
	if err := db.QueryRow(`SELECT ts, tz, d FROM t`).Scan(&ts, &tz, &d); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !ts.Equal(when) || !tz.Equal(when) {
		t.Errorf("timestamps = %v, %v, want %v", ts, tz, when)
	}
	// A date drops the time of day rather than keeping it.
	if want := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC); !d.Equal(want) {
		t.Errorf("date = %v, want %v", d, want)
	}
}

// TestExplicitCast checks CAST and its :: spelling, which parsed but were never
// bound, so every cast failed at execution.
func TestExplicitCast(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (a INT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (42)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	tests := []struct {
		name string
		sql  string
		want string
	}{
		{"CAST of a column", `SELECT CAST(a AS TEXT) FROM t`, "42"},
		{"the :: spelling", `SELECT a::TEXT FROM t`, "42"},
		{"text to integer", `SELECT CAST('7' AS INT) + 1`, "8"},
		{"a cast binds tighter than arithmetic", `SELECT 1 + '2'::INT`, "3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			if err := db.QueryRow(tt.sql).Scan(&got); err != nil {
				t.Fatalf("%s: %v", tt.sql, err)
			}
			if got != tt.want {
				t.Errorf("%s = %q, want %q", tt.sql, got, tt.want)
			}
		})
	}

	if _, err := db.Query(`SELECT CAST('abc' AS INT)`); err == nil {
		t.Error("an impossible cast succeeded")
	}
}
