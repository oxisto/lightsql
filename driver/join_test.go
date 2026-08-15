package driver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// joinFixture is two tables with a deliberately partial overlap: e2 has no
// department, and d3 has no employee. That is what makes the four outer
// flavours produce four different answers.
var joinFixture = []string{
	`CREATE TABLE emp (id INT, name TEXT, dept INT)`,
	`CREATE TABLE dept (id INT, label TEXT)`,
	`INSERT INTO emp VALUES (1, 'ada', 10), (2, 'bob', NULL), (3, 'cy', 20)`,
	`INSERT INTO dept VALUES (10, 'eng'), (20, 'ops'), (30, 'hr')`,
}

// rowsOf runs a query and renders each row as a single string, so a whole
// result set can be asserted at once rather than field by field.
func rowsOf(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			switch v := c.(type) {
			case nil:
				parts[i] = "NULL"
			case []byte:
				parts[i] = string(v)
			default:
				parts[i] = fmt.Sprint(v)
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestJoinTypes(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, joinFixture...)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			// Only the pairs that match. bob has no department and hr has no
			// employee, so neither appears.
			name:  "inner",
			query: `SELECT emp.name, dept.label FROM emp INNER JOIN dept ON emp.dept = dept.id ORDER BY emp.name`,
			want:  []string{"ada|eng", "cy|ops"},
		},
		{
			// Every employee survives; bob is padded with NULL. This is the
			// case that a plain filter cannot express.
			name:  "left",
			query: `SELECT emp.name, dept.label FROM emp LEFT JOIN dept ON emp.dept = dept.id ORDER BY emp.name`,
			want:  []string{"ada|eng", "bob|NULL", "cy|ops"},
		},
		{
			// Every department survives; hr is padded on the left.
			name:  "right",
			query: `SELECT emp.name, dept.label FROM emp RIGHT JOIN dept ON emp.dept = dept.id ORDER BY dept.label`,
			want:  []string{"ada|eng", "NULL|hr", "cy|ops"},
		},
		{
			// Both unmatched rows survive, one padded on each side.
			name:  "full",
			query: `SELECT emp.name, dept.label FROM emp FULL JOIN dept ON emp.dept = dept.id ORDER BY emp.name, dept.label`,
			// NULLs sort last in ASC, so the right-only row comes last rather
			// than first.
			want: []string{"ada|eng", "bob|NULL", "cy|ops", "NULL|hr"},
		},
		{
			// OUTER is noise after LEFT/RIGHT/FULL and must parse the same.
			name:  "left outer is the same as left",
			query: `SELECT emp.name, dept.label FROM emp LEFT OUTER JOIN dept ON emp.dept = dept.id ORDER BY emp.name`,
			want:  []string{"ada|eng", "bob|NULL", "cy|ops"},
		},
		{
			name:  "cross",
			query: `SELECT emp.name, dept.label FROM emp CROSS JOIN dept ORDER BY emp.name, dept.label`,
			want: []string{
				"ada|eng", "ada|hr", "ada|ops",
				"bob|eng", "bob|hr", "bob|ops",
				"cy|eng", "cy|hr", "cy|ops",
			},
		},
		{
			// A comma in FROM is a cross join, and must produce the same rows.
			name:  "comma is a cross join",
			query: `SELECT emp.name, dept.label FROM emp, dept ORDER BY emp.name, dept.label LIMIT 3`,
			want:  []string{"ada|eng", "ada|hr", "ada|ops"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rowsOf(t, db, tt.query)
			if !slices.Equal(got, tt.want) {
				t.Errorf("got  %v\nwant %v", got, tt.want)
			}
		})
	}
}

// TestJoinNullNeverMatches is the three-valued-logic case. bob's dept is NULL,
// and NULL = anything is unknown, so he matches nothing even though there is a
// department whose id is also absent from nowhere. Only true joins two rows.
func TestJoinNullNeverMatches(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE a (v INT)`,
		`CREATE TABLE b (v INT)`,
		`INSERT INTO a VALUES (NULL), (1)`,
		`INSERT INTO b VALUES (NULL), (1)`,
	)
	got := rowsOf(t, db, `SELECT a.v, b.v FROM a JOIN b ON a.v = b.v`)
	if !slices.Equal(got, []string{"1|1"}) {
		t.Errorf("got %v, want [1|1] — NULL must not match NULL", got)
	}
}

func TestJoinUsing(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE l (id INT, x TEXT)`,
		`CREATE TABLE r (id INT, y TEXT)`,
		`INSERT INTO l VALUES (1, 'lx'), (2, 'ly')`,
		`INSERT INTO r VALUES (1, 'rx'), (3, 'rz')`,
	)

	t.Run("joins on the named column", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT x, y FROM l JOIN r USING (id)`)
		if !slices.Equal(got, []string{"lx|rx"}) {
			t.Errorf("got %v, want [lx|rx]", got)
		}
	})

	t.Run("the merged column is not ambiguous", func(t *testing.T) {
		// Unqualified id would match two columns without the merge, and SQL
		// rejects an ambiguous reference rather than picking one.
		got := rowsOf(t, db, `SELECT id, x FROM l JOIN r USING (id)`)
		if !slices.Equal(got, []string{"1|lx"}) {
			t.Errorf("got %v, want [1|lx]", got)
		}
	})

	t.Run("star shows the merged column once", func(t *testing.T) {
		rows, err := db.Query(`SELECT * FROM l JOIN r USING (id)`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"id", "x", "y"}
		if !slices.Equal(cols, want) {
			t.Errorf("columns = %v, want %v", cols, want)
		}
	})

	t.Run("the hidden copy stays reachable when qualified", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT r.id FROM l JOIN r USING (id)`)
		if !slices.Equal(got, []string{"1"}) {
			t.Errorf("got %v, want [1]", got)
		}
	})

	t.Run("an unknown column is rejected", func(t *testing.T) {
		if _, err := db.Query(`SELECT 1 FROM l JOIN r USING (nope)`); err == nil {
			t.Error("USING (nope) was accepted")
		} else if got := sqlstate(err); got != "42703" {
			t.Errorf("SQLSTATE = %q, want 42703 (undefined_column)", got)
		}
	})
}

// TestJoinThreeTables checks that a join can be the left side of another join,
// which is where the ordinal arithmetic would show up if it were wrong.
func TestJoinThreeTables(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE a (id INT, av TEXT)`,
		`CREATE TABLE b (id INT, bv TEXT)`,
		`CREATE TABLE c (id INT, cv TEXT)`,
		`INSERT INTO a VALUES (1, 'a1'), (2, 'a2')`,
		`INSERT INTO b VALUES (1, 'b1'), (2, 'b2')`,
		`INSERT INTO c VALUES (1, 'c1')`,
	)

	got := rowsOf(t, db, `
		SELECT a.av, b.bv, c.cv
		FROM a JOIN b ON a.id = b.id JOIN c ON b.id = c.id`)
	if !slices.Equal(got, []string{"a1|b1|c1"}) {
		t.Errorf("got %v, want [a1|b1|c1]", got)
	}

	// The third table joined as a LEFT keeps both a/b pairs.
	got = rowsOf(t, db, `
		SELECT a.av, c.cv
		FROM a JOIN b ON a.id = b.id LEFT JOIN c ON b.id = c.id
		ORDER BY a.av`)
	if !slices.Equal(got, []string{"a1|c1", "a2|NULL"}) {
		t.Errorf("got %v, want [a1|c1 a2|NULL]", got)
	}
}

// TestSelfJoin needs aliases to distinguish the two instances, and is the case
// where a shared scope would collide if qualifiers were not per instance.
func TestSelfJoin(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE node (id INT, parent INT, label TEXT)`,
		`INSERT INTO node VALUES (1, NULL, 'root'), (2, 1, 'child'), (3, 2, 'leaf')`,
	)
	got := rowsOf(t, db, `
		SELECT child.label, parent.label
		FROM node child JOIN node parent ON child.parent = parent.id
		ORDER BY child.label`)
	if !slices.Equal(got, []string{"child|root", "leaf|child"}) {
		t.Errorf("got %v, want [child|root leaf|child]", got)
	}
}

func TestJoinWithWhereAndProjection(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, joinFixture...)

	// WHERE runs above the join, so it can filter on either side, and it drops
	// the NULL-padded row that a LEFT JOIN produced — the classic difference
	// between a condition in ON and one in WHERE.
	got := rowsOf(t, db, `
		SELECT emp.name FROM emp LEFT JOIN dept ON emp.dept = dept.id
		WHERE dept.label = 'eng'`)
	if !slices.Equal(got, []string{"ada"}) {
		t.Errorf("got %v, want [ada]", got)
	}

	// The same predicate in ON keeps every left row instead.
	got = rowsOf(t, db, `
		SELECT emp.name, dept.label FROM emp LEFT JOIN dept
		  ON emp.dept = dept.id AND dept.label = 'eng'
		ORDER BY emp.name`)
	if !slices.Equal(got, []string{"ada|eng", "bob|NULL", "cy|NULL"}) {
		t.Errorf("got %v, want [ada|eng bob|NULL cy|NULL]", got)
	}
}

func TestJoinErrors(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, joinFixture...)

	tests := []struct {
		name  string
		query string
		state string
	}{
		{"missing ON", `SELECT 1 FROM emp JOIN dept`, "42601"},
		{"unknown table", `SELECT 1 FROM emp JOIN nope ON emp.id = nope.id`, "42P01"},
		{"unknown column in ON", `SELECT 1 FROM emp JOIN dept ON emp.zzz = dept.id`, "42703"},
		{"ambiguous unqualified column", `SELECT id FROM emp JOIN dept ON emp.dept = dept.id`, "42702"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Query(tt.query)
			if err == nil {
				t.Fatalf("%s was accepted", tt.query)
			}
			if got := sqlstate(err); got != tt.state {
				t.Errorf("SQLSTATE = %q, want %q (%v)", got, tt.state, err)
			}
		})
	}
}

// TestJoinHonoursCancellation checks that the cancellation check sits inside
// the join loop, not only at statement entry. A cross join is the cheapest way
// to build a plan that would otherwise run far longer than the caller wants.
func TestJoinHonoursCancellation(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, `CREATE TABLE a (v INT)`, `CREATE TABLE b (v INT)`)
	for i := range 200 {
		db.Exec(`INSERT INTO a VALUES ($1)`, i)
		db.Exec(`INSERT INTO b VALUES ($1)`, i)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.QueryContext(ctx, `SELECT a.v FROM a CROSS JOIN b`); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled cross join returned %v, want context.Canceled", err)
	}
}
