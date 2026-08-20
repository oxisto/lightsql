package driver_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
)

var insertSelectFixture = []string{
	`CREATE TABLE src (id INT, name TEXT, n INT)`,
	`CREATE TABLE dst (id INT, name TEXT, n INT)`,
	`INSERT INTO src VALUES (1, 'ada', 10), (2, 'bob', NULL), (3, 'cy', 30)`,
}

func TestInsertSelect(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, insertSelectFixture...)

	res, err := db.Exec(`INSERT INTO dst SELECT id, name, n FROM src WHERE n IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Errorf("affected = %d, want 2", n)
	}
	got := rowsOf(t, db, `SELECT id, name, n FROM dst ORDER BY id`)
	if want := []string{"1|ada|10", "3|cy|30"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestInsertSelectColumnList(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, insertSelectFixture...)

	// The target list picks which columns are filled and in what order; the
	// rest stay NULL.
	if _, err := db.Exec(`INSERT INTO dst (name, id) SELECT name, id FROM src ORDER BY id`); err != nil {
		t.Fatal(err)
	}
	got := rowsOf(t, db, `SELECT id, name, n FROM dst ORDER BY id`)
	want := []string{"1|ada|NULL", "2|bob|NULL", "3|cy|NULL"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestInsertSelectFromItself is the case where a naive implementation does not
// terminate: the statement reads the table it is appending to. A scan takes its
// rows when the operator is built, so the statement sees the table as it was.
func TestInsertSelectFromItself(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, insertSelectFixture...)

	res, err := db.Exec(`INSERT INTO src SELECT id + 100, name, n FROM src`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 3 {
		t.Errorf("affected = %d, want 3", n)
	}
	got := rowsOf(t, db, `SELECT count(*) FROM src`)
	if want := []string{"6"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestInsertSelectFeatures(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, insertSelectFixture...)

	tests := []struct {
		name string
		stmt string
		want []string
	}{
		{
			// The source may be any query, not only a plain scan.
			name: "aggregate source",
			stmt: `INSERT INTO dst (id, n) SELECT count(*), sum(n) FROM src`,
			want: []string{"3|NULL|40"},
		},
		{
			name: "expression source",
			stmt: `INSERT INTO dst (id, name, n) SELECT id * 2, name || '!', n FROM src WHERE id = 1`,
			want: []string{"2|ada!|10"},
		},
		{
			name: "source with a join",
			stmt: `INSERT INTO dst (id, name, n) SELECT a.id, b.name, a.n FROM src a JOIN src b ON a.id = b.id WHERE a.id = 3`,
			want: []string{"3|cy|30"},
		},
		{
			name: "source with limit",
			stmt: `INSERT INTO dst (id, name, n) SELECT id, name, n FROM src ORDER BY id LIMIT 1`,
			want: []string{"1|ada|10"},
		},
		{
			name: "source with no rows inserts nothing",
			stmt: `INSERT INTO dst SELECT id, name, n FROM src WHERE id = 999`,
			want: nil,
		},
		{
			// A subquery in the select list of the source.
			name: "scalar subquery in the source",
			stmt: `INSERT INTO dst (id, n) SELECT id, (SELECT count(*) FROM src) FROM src WHERE id = 2`,
			want: []string{"2|NULL|3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := db.Exec(`DELETE FROM dst`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(tt.stmt); err != nil {
				t.Fatalf("%s: %v", tt.stmt, err)
			}
			got := rowsOf(t, db, `SELECT id, name, n FROM dst ORDER BY id`)
			if !slices.Equal(got, tt.want) {
				t.Errorf("%s\n got: %v\nwant: %v", tt.stmt, got, tt.want)
			}
		})
	}
}

func TestInsertSelectSerialAndDefault(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE gen (id SERIAL, label TEXT, tag TEXT DEFAULT 'plain')`,
		`CREATE TABLE seed (label TEXT)`,
		`INSERT INTO seed VALUES ('a'), ('b')`)

	// A serial and a DEFAULT must be filled for source rows exactly as they are
	// for a VALUES row -- and a serial must advance per row rather than being
	// evaluated once for the statement.
	if _, err := db.Exec(`INSERT INTO gen (label) SELECT label FROM seed ORDER BY label`); err != nil {
		t.Fatal(err)
	}
	got := rowsOf(t, db, `SELECT id, label, tag FROM gen ORDER BY id`)
	if want := []string{"1|a|plain", "2|b|plain"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestInsertSelectReturningAndChecks(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE checked (n INT CHECK (n > 0))`,
		`CREATE TABLE nums (n INT)`,
		`INSERT INTO nums VALUES (1), (2), (-1)`)

	// A CHECK applies to rows arriving from a SELECT too.
	_, err := db.Exec(`INSERT INTO checked SELECT n FROM nums`)
	if err == nil {
		t.Fatal("expected the CHECK to reject the negative row")
	}
	if !strings.Contains(err.Error(), "check") {
		t.Errorf("got %v, want a check violation", err)
	}

	// RETURNING works over a SELECT source.
	rows, err := db.Query(`INSERT INTO checked SELECT n FROM nums WHERE n > 0 RETURNING n`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []int{1, 2}; !slices.Equal(got, want) {
		t.Errorf("RETURNING got %v, want %v", got, want)
	}
}

func TestInsertSelectErrors(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, insertSelectFixture...)

	tests := []struct {
		name string
		stmt string
		want string
	}{
		{
			name: "too few columns",
			stmt: `INSERT INTO dst (id, name) SELECT id FROM src`,
			want: "1 expressions but 2 target columns",
		},
		{
			name: "too many columns",
			stmt: `INSERT INTO dst (id) SELECT id, name FROM src`,
			want: "2 expressions but 1 target columns",
		},
		{
			// The same type rule the VALUES form applies.
			name: "incompatible column type",
			stmt: `INSERT INTO dst (id) SELECT name FROM src`,
			want: "cannot be used where",
		},
		{
			name: "unknown source table",
			stmt: `INSERT INTO dst SELECT id, name, n FROM nope`,
			want: "does not exist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.Exec(tt.stmt)
			if err == nil {
				t.Fatalf("%s: expected an error", tt.stmt)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s\n got: %v\nwant it to contain: %q", tt.stmt, err, tt.want)
			}
		})
	}
}

// TestInsertSelectRollback confirms a failed INSERT ... SELECT leaves nothing
// behind, since it writes many rows and fails partway through.
func TestInsertSelectRollback(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE checked (n INT CHECK (n > 0))`,
		`CREATE TABLE nums (n INT)`,
		`INSERT INTO nums VALUES (1), (2), (-1)`)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO checked SELECT n FROM nums`); err == nil {
		t.Fatal("expected the CHECK to reject the negative row")
	}
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatal(err)
	}

	if got := rowsOf(t, db, `SELECT count(*) FROM checked`); !slices.Equal(got, []string{"0"}) {
		t.Errorf("after rollback: got %v, want no rows", got)
	}
}
