package driver_test

import (
	"slices"
	"strings"
	"testing"
	"time"
)

var fnFixture = []string{
	`CREATE TABLE t (id INT, s TEXT, n INT)`,
	`INSERT INTO t VALUES (1, 'Ada', 10), (2, NULL, NULL), (3, '  cy  ', -5)`,
}

func TestScalarFunctions(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, fnFixture...)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"lower", `SELECT lower('ABC')`, []string{"abc"}},
		{"upper", `SELECT upper('abc')`, []string{"ABC"}},
		{"trim", `SELECT trim('  x  ')`, []string{"x"}},
		// Characters, not bytes: counting bytes would make this 6.
		{"length counts characters", `SELECT length('héllo')`, []string{"5"}},
		{"abs of an integer stays an integer", `SELECT abs(-5)`, []string{"5"}},
		{"abs of a float", `SELECT abs(-5.5)`, []string{"5.5"}},
		{"round", `SELECT round(2.6)`, []string{"3"}},

		// Every scalar function but coalesce and nullif propagates NULL, and
		// that check lives in one place rather than in each implementation.
		{"null propagates", `SELECT lower(s) IS NULL FROM t WHERE id = 2`, []string{"true"}},
		{"over a column", `SELECT lower(s) FROM t WHERE id = 1`, []string{"ada"}},

		{"coalesce picks the first non-null", `SELECT coalesce(NULL, NULL, 3)`, []string{"3"}},
		// All-NULL is NULL, not an error and not a zero value.
		{"coalesce of all nulls", `SELECT coalesce(NULL, NULL) IS NULL`, []string{"true"}},
		{"coalesce over a column", `SELECT coalesce(n, 0) FROM t ORDER BY id`, []string{"10", "0", "-5"}},

		{"nullif when equal", `SELECT nullif(1, 1) IS NULL`, []string{"true"}},
		{"nullif when different", `SELECT nullif(1, 2)`, []string{"1"}},

		// A function is an ordinary expression, so it composes with the rest.
		{"in a where clause", `SELECT id FROM t WHERE lower(s) = 'ada'`, []string{"1"}},
		{"inside an aggregate", `SELECT sum(coalesce(n, 0)) FROM t`, []string{"5"}},
		{"nested", `SELECT upper(trim('  x  '))`, []string{"X"}},
		{"in a group key", `SELECT length(coalesce(s, '')), count(*) FROM t GROUP BY length(coalesce(s, '')) ORDER BY 1`,
			[]string{"0|1", "3|1", "6|1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rowsOf(t, db, tt.query); !slices.Equal(got, tt.want) {
				t.Errorf("%s\n got: %v\nwant: %v", tt.query, got, tt.want)
			}
		})
	}
}

// TestScalarArgumentTypes is the check that matters most here. A Value keeps its
// payload in the same field whatever the kind, so a text function handed a
// number does not fail — it reads the empty string payload and answers
// confidently. lower(1) returned "" and length(1) returned 0 before the binder
// refused them.
func TestScalarArgumentTypes(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, fnFixture...)

	rejected := []string{
		`SELECT lower(1)`,
		`SELECT upper(1)`,
		`SELECT length(1)`,
		`SELECT trim(1)`,
		`SELECT abs('x')`,
		`SELECT round('x')`,
	}
	for _, q := range rejected {
		t.Run(q, func(t *testing.T) {
			err := queryErr(db, q)
			if err == nil {
				t.Fatalf("%s: expected a type error", q)
			}
			if !strings.Contains(err.Error(), "does not exist") {
				t.Errorf("got %v, want it to name the function and its argument types", err)
			}
		})
	}

	// The other direction: a function that legitimately takes anything must
	// keep doing so, or this check would have gone too far.
	for _, q := range []string{
		`SELECT coalesce(NULL, 1)`,
		`SELECT coalesce(NULL, 'x')`,
		`SELECT nullif('a', 'b')`,
		`SELECT nullif(1, 2)`,
		`SELECT lower(NULL) IS NULL`,
	} {
		t.Run(q, func(t *testing.T) {
			if err := queryErr(db, q); err != nil {
				t.Errorf("%s: unexpectedly rejected: %v", q, err)
			}
		})
	}
}

func TestScalarArity(t *testing.T) {
	db := open(t)

	for _, q := range []string{
		`SELECT lower('a', 'b')`,
		`SELECT nullif(1)`,
		`SELECT now(1)`,
		`SELECT nosuchfunction(1)`,
	} {
		t.Run(q, func(t *testing.T) {
			if err := queryErr(db, q); err == nil {
				t.Errorf("%s: expected an error", q)
			}
		})
	}
}

// TestNow pins that now() reports the transaction's start rather than the wall
// clock, so every statement in one transaction agrees about the time. Without
// that, two rows written by the same transaction could disagree about when.
func TestNow(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, `CREATE TABLE ev (id INT, at TIMESTAMPTZ DEFAULT now())`)

	before := time.Now().UTC().Add(-time.Minute)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO ev (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	// A separate statement in the same transaction must see the same instant.
	var same bool
	if err := tx.QueryRow(`SELECT now() = (SELECT at FROM ev WHERE id = 1)`).Scan(&same); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Error("now() differed between two statements of one transaction")
	}

	var at time.Time
	if err := db.QueryRow(`SELECT at FROM ev WHERE id = 1`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at.Before(before) || at.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("now() = %v, which is not close to the present", at)
	}
}
