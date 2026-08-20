package driver_test

import (
	"slices"
	"strings"
	"testing"
)

var textFixture = []string{
	`CREATE TABLE t (id INT, s TEXT)`,
	`INSERT INTO t VALUES (1, 'ada'), (2, 'bob'), (3, 'cy'), (4, NULL), (5, '100%'), (6, 'a.c')`,
}

func TestBetween(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, textFixture...)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			// BETWEEN is inclusive at both ends, which is the whole reason it
			// is not a pair of strict comparisons.
			name:  "inclusive at both ends",
			query: `SELECT id FROM t WHERE id BETWEEN 2 AND 4 ORDER BY id`,
			want:  []string{"2", "3", "4"},
		},
		{
			name:  "not between",
			query: `SELECT id FROM t WHERE id NOT BETWEEN 2 AND 4 ORDER BY id`,
			want:  []string{"1", "5", "6"},
		},
		{
			// An empty range yields nothing rather than reversing itself.
			name:  "reversed bounds match nothing",
			query: `SELECT id FROM t WHERE id BETWEEN 4 AND 2`,
			want:  nil,
		},
		{
			// id 4 has a NULL s. NULL is unknown, and unknown does not pass a
			// filter, so it appears in neither BETWEEN nor NOT BETWEEN below.
			// id 5 is '100%', which sorts before 'a' and so is legitimately out
			// of range rather than unknown.
			name:  "null is in neither",
			query: `SELECT id FROM t WHERE s BETWEEN 'a' AND 'z' ORDER BY id`,
			want:  []string{"1", "2", "3", "6"},
		},
		{
			name:  "null is in neither, negated",
			query: `SELECT id FROM t WHERE s NOT BETWEEN 'a' AND 'z' ORDER BY id`,
			want:  []string{"5"},
		},
		{
			// A NULL bound makes the whole thing unknown.
			name:  "null bound",
			query: `SELECT id FROM t WHERE id BETWEEN 1 AND NULL`,
			want:  nil,
		},
		{
			name:  "over text",
			query: `SELECT s FROM t WHERE s BETWEEN 'ada' AND 'bob' ORDER BY s`,
			want:  []string{"ada", "bob"},
		},
		{
			name:  "bounds may be expressions",
			query: `SELECT id FROM t WHERE id BETWEEN 1 + 1 AND 2 * 2 ORDER BY id`,
			want:  []string{"2", "3", "4"},
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

// TestLike covers the operator that previously fell through to the arithmetic
// path, where the default branch is exponentiation: both strings converted to
// 0, 0 ** 0 is 1, so every LIKE answered the float 1 and every WHERE built on
// one silently matched nothing.
func TestLike(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, textFixture...)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "prefix",
			query: `SELECT s FROM t WHERE s LIKE 'a%' ORDER BY s`,
			want:  []string{"a.c", "ada"},
		},
		{
			name:  "suffix",
			query: `SELECT s FROM t WHERE s LIKE '%y' ORDER BY s`,
			want:  []string{"cy"},
		},
		{
			name:  "underscore matches exactly one character",
			query: `SELECT s FROM t WHERE s LIKE 'c_' ORDER BY s`,
			want:  []string{"cy"},
		},
		{
			name:  "underscore does not match two",
			query: `SELECT s FROM t WHERE s LIKE 'a_' ORDER BY s`,
			want:  nil,
		},
		{
			// The pattern is anchored: LIKE matches the whole string, not a
			// substring, so this finds nothing without a leading %.
			name:  "anchored at both ends",
			query: `SELECT s FROM t WHERE s LIKE 'd'`,
			want:  nil,
		},
		{
			name:  "contains",
			query: `SELECT s FROM t WHERE s LIKE '%d%' ORDER BY s`,
			want:  []string{"ada"},
		},
		{
			name:  "no wildcards is equality",
			query: `SELECT s FROM t WHERE s LIKE 'bob'`,
			want:  []string{"bob"},
		},
		{
			// A dot is a literal character, not a regular expression's any.
			// This is what QuoteMeta is protecting.
			name:  "dot is literal",
			query: `SELECT s FROM t WHERE s LIKE 'a.c'`,
			want:  []string{"a.c"},
		},
		{
			name:  "dot does not match an arbitrary character",
			query: `SELECT s FROM t WHERE s LIKE 'a.a'`,
			want:  nil,
		},
		{
			// A backslash escapes the wildcard, so this matches the literal
			// per cent sign rather than anything beginning with 100.
			name:  "escaped percent",
			query: `SELECT s FROM t WHERE s LIKE '100\%'`,
			want:  []string{"100%"},
		},
		{
			name:  "not like",
			query: `SELECT s FROM t WHERE s NOT LIKE 'a%' ORDER BY s`,
			want:  []string{"100%", "bob", "cy"},
		},
		{
			// NULL is unknown on either side, so the row is in neither LIKE nor
			// NOT LIKE -- the same rule a comparison follows.
			name:  "null is in neither",
			query: `SELECT count(*) FROM t WHERE s LIKE '%' OR s NOT LIKE '%'`,
			want:  []string{"5"},
		},
		{
			name:  "null pattern is unknown",
			query: `SELECT count(*) FROM t WHERE s LIKE NULL`,
			want:  []string{"0"},
		},
		{
			// A LIKE is an ordinary boolean expression, not only a filter.
			name:  "in the select list",
			query: `SELECT s LIKE 'a%' FROM t WHERE id = 1`,
			want:  []string{"true"},
		},
		{
			// The pattern need not be constant, in which case it is compiled
			// per row rather than once.
			name:  "pattern from a column",
			query: `SELECT s FROM t WHERE 'ada' LIKE s ORDER BY s`,
			want:  []string{"ada"},
		},
		{
			name:  "pattern from a parameter",
			query: `SELECT s FROM t WHERE s LIKE 'c%' ORDER BY s`,
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

func TestLikeParameter(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, textFixture...)

	rows, err := db.Query(`SELECT s FROM t WHERE s LIKE $1 ORDER BY s`, "a%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.c", "ada"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLikeErrors(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, textFixture...)

	// LIKE is defined over strings. Accepting a number would mean falling back
	// to whatever the generic path did with it, which is how this operator
	// silently answered 1 for every row.
	err := queryErr(db, `SELECT s FROM t WHERE id LIKE 1`)
	if err == nil {
		t.Fatal("expected an error for LIKE over integers")
	}
	if !strings.Contains(err.Error(), "cannot be used where") {
		t.Errorf("got %v, want a type error", err)
	}
}
