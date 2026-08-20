package driver_test

import (
	"slices"
	"strings"
	"testing"
)

var caseFixture = []string{
	`CREATE TABLE emp (id INT, name TEXT, dept INT, rate DOUBLE PRECISION)`,
	`INSERT INTO emp VALUES (1, 'ada', 10, 1.5), (2, 'bob', NULL, 2.0), (3, 'cy', 20, 2.5)`,
}

func TestCaseExpr(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, caseFixture...)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "searched form",
			query: `SELECT CASE WHEN id = 1 THEN 'one' ELSE 'other' END FROM emp ORDER BY id`,
			want:  []string{"one", "other", "other"},
		},
		{
			name:  "simple form",
			query: `SELECT CASE id WHEN 1 THEN 'one' WHEN 2 THEN 'two' ELSE 'many' END FROM emp ORDER BY id`,
			want:  []string{"one", "two", "many"},
		},
		{
			// A CASE that matches nothing and has no ELSE is NULL, not an
			// error and not a zero value.
			name:  "no match and no else is null",
			query: `SELECT CASE WHEN id = 99 THEN 'x' END FROM emp ORDER BY id`,
			want:  []string{"NULL", "NULL", "NULL"},
		},
		{
			// Only true fires an arm. NULL is unknown, so it is skipped like
			// false, and the ELSE wins.
			name:  "unknown condition does not fire",
			query: `SELECT CASE WHEN NULL THEN 'yes' ELSE 'no' END`,
			want:  []string{"no"},
		},
		{
			// The first matching arm wins even when a later one also matches.
			name:  "first match wins",
			query: `SELECT CASE WHEN id >= 1 THEN 'low' WHEN id >= 2 THEN 'high' END FROM emp ORDER BY id`,
			want:  []string{"low", "low", "low"},
		},
		{
			// A NULL operand matches no arm of a simple CASE, because NULL is
			// not equal to anything -- including NULL.
			name:  "null operand matches nothing",
			query: `SELECT name, CASE dept WHEN 10 THEN 'eng' ELSE 'other' END FROM emp ORDER BY name`,
			want:  []string{"ada|eng", "bob|other", "cy|other"},
		},
		{
			// An operand of unknown type matches no arm, so the ELSE wins. It
			// must not be a bind error: converting the arm to the operand's
			// type would be asking for the integer 1 as a NULL.
			name:  "null operand with a typed arm",
			query: `SELECT CASE NULL WHEN 1 THEN 'a' ELSE 'b' END`,
			want:  []string{"b"},
		},
		{
			name:  "null operand with no else is null",
			query: `SELECT CASE NULL WHEN 1 THEN 'a' END IS NULL`,
			want:  []string{"true"},
		},
		{
			// The mirror image, which already worked: a NULL arm against a
			// typed operand.
			name:  "null arm with a typed operand",
			query: `SELECT CASE 1 WHEN NULL THEN 'a' ELSE 'b' END`,
			want:  []string{"b"},
		},
		{
			// A NULL branch does not constrain the type of the others.
			name:  "null branch keeps the other type",
			query: `SELECT CASE WHEN id = 1 THEN NULL ELSE 'x' END FROM emp ORDER BY id`,
			want:  []string{"NULL", "x", "x"},
		},
		{
			// Mixed numeric branches promote to float, the same as arithmetic
			// over those values would.
			name:  "numeric branches promote",
			query: `SELECT CASE WHEN id = 1 THEN 1 ELSE rate END FROM emp ORDER BY id`,
			want:  []string{"1", "2", "2.5"},
		},
		{
			name:  "nested case",
			query: `SELECT CASE WHEN id < 3 THEN CASE WHEN id = 1 THEN 'a' ELSE 'b' END ELSE 'c' END FROM emp ORDER BY id`,
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "in a where clause",
			query: `SELECT name FROM emp WHERE CASE WHEN dept IS NULL THEN false ELSE dept > 15 END`,
			want:  []string{"cy"},
		},
		{
			// A CASE is an ordinary expression, so it can be grouped by and
			// aggregated over.
			name:  "grouped by a case",
			query: `SELECT CASE WHEN dept IS NULL THEN 'none' ELSE 'some' END AS g, count(*) FROM emp GROUP BY CASE WHEN dept IS NULL THEN 'none' ELSE 'some' END ORDER BY g`,
			want:  []string{"none|1", "some|2"},
		},
		{
			// Only the arm that fires is evaluated, so the division by zero in
			// the untaken branch never happens.
			name:  "untaken branch is not evaluated",
			query: `SELECT CASE WHEN id > 0 THEN 0 ELSE 1 / 0 END FROM emp ORDER BY id`,
			want:  []string{"0", "0", "0"},
		},
		{
			name:  "case over a subquery",
			query: `SELECT CASE WHEN (SELECT count(*) FROM emp) = 3 THEN 'three' ELSE 'other' END`,
			want:  []string{"three"},
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

func TestCaseErrors(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, caseFixture...)

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			// Branches that disagree would give a result column whose type
			// depends on which row you look at.
			name:  "branches of different types",
			query: `SELECT CASE WHEN id = 1 THEN 'text' ELSE 1 END FROM emp`,
			want:  "cannot be matched",
		},
		{
			// The searched form tests, so its arm must be a predicate.
			name:  "non-boolean condition",
			query: `SELECT CASE WHEN name THEN 1 ELSE 2 END FROM emp`,
			want:  "must be boolean",
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
