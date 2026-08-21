package driver_test

import (
	"slices"
	"strings"
	"testing"
)

func TestOnConflictDoNothing(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE access (user_id TEXT, person_id TEXT, PRIMARY KEY (user_id, person_id))`,
		`INSERT INTO access VALUES ('u', 'p')`,
	)

	res, err := db.Exec(`INSERT INTO access VALUES ('u', 'p') ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}
	// Zero rows affected is how a caller tells a skip from a write.
	if n, _ := res.RowsAffected(); n != 0 {
		t.Errorf("affected = %d, want 0", n)
	}
	if got := rowsOf(t, db, `SELECT count(*) FROM access`); !slices.Equal(got, []string{"1"}) {
		t.Errorf("got %v rows, want 1", got)
	}

	// A row that does not collide is still inserted normally.
	res, err = db.Exec(`INSERT INTO access VALUES ('u', 'q') ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("affected = %d, want 1", n)
	}

	// With an explicit target too.
	if _, err := db.Exec(`INSERT INTO access VALUES ('u', 'p') ON CONFLICT (user_id, person_id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, db, `SELECT count(*) FROM access`); !slices.Equal(got, []string{"2"}) {
		t.Errorf("got %v rows, want 2", got)
	}
}

func TestOnConflictDoUpdate(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE quotes (listing_id TEXT, time INT, price INT, PRIMARY KEY (listing_id, time))`,
		`INSERT INTO quotes VALUES ('a', 1, 100)`,
	)

	// The upsert money-gopher writes.
	if _, err := db.Exec(
		`INSERT INTO quotes VALUES ('a', 1, 200) ON CONFLICT (listing_id, time) DO UPDATE SET price = excluded.price`,
	); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, db, `SELECT price FROM quotes WHERE listing_id = 'a'`); !slices.Equal(got, []string{"200"}) {
		t.Errorf("got %v, want [200]", got)
	}

	// A non-colliding row takes the insert path.
	if _, err := db.Exec(
		`INSERT INTO quotes VALUES ('b', 1, 50) ON CONFLICT (listing_id, time) DO UPDATE SET price = excluded.price`,
	); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, db, `SELECT listing_id, price FROM quotes ORDER BY listing_id`); !slices.Equal(got, []string{"a|200", "b|50"}) {
		t.Errorf("got %v", got)
	}

	// The update sees both rows: the stored one by table name, the proposed one
	// as excluded. This is what the clause is for, and what a handler that only
	// copied the proposed row would get wrong.
	if _, err := db.Exec(
		`INSERT INTO quotes VALUES ('a', 1, 7) ON CONFLICT (listing_id, time) DO UPDATE SET price = quotes.price + excluded.price`,
	); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, db, `SELECT price FROM quotes WHERE listing_id = 'a'`); !slices.Equal(got, []string{"207"}) {
		t.Errorf("got %v, want [207] (200 stored + 7 proposed)", got)
	}
}

func TestOnConflictDoUpdateWhere(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE q (id INT PRIMARY KEY, price INT)`,
		`INSERT INTO q VALUES (1, 100)`,
	)

	// The predicate excludes the row, so neither the update nor the insert
	// happens and the stored value stands.
	res, err := db.Exec(`INSERT INTO q VALUES (1, 50) ON CONFLICT (id) DO UPDATE SET price = excluded.price WHERE excluded.price > q.price`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Errorf("affected = %d, want 0", n)
	}
	if got := rowsOf(t, db, `SELECT price FROM q`); !slices.Equal(got, []string{"100"}) {
		t.Errorf("got %v, want [100]", got)
	}

	// A higher price passes the predicate.
	if _, err := db.Exec(`INSERT INTO q VALUES (1, 150) ON CONFLICT (id) DO UPDATE SET price = excluded.price WHERE excluded.price > q.price`); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, db, `SELECT price FROM q`); !slices.Equal(got, []string{"150"}) {
		t.Errorf("got %v, want [150]", got)
	}
}

func TestOnConflictReturning(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE q (id INT PRIMARY KEY, price INT)`,
		`INSERT INTO q VALUES (1, 100)`,
	)

	// RETURNING reports the row as it now stands, which for DO UPDATE is the
	// updated row rather than the one that was proposed.
	var price int
	err := db.QueryRow(
		`INSERT INTO q VALUES (1, 5) ON CONFLICT (id) DO UPDATE SET price = q.price + excluded.price RETURNING price`,
	).Scan(&price)
	if err != nil {
		t.Fatal(err)
	}
	if price != 105 {
		t.Errorf("RETURNING gave %d, want 105", price)
	}
}

// TestOnConflictUniqueIndexArbiter confirms a unique index serves as an
// arbiter, since that is how PostgreSQL enforces uniqueness in the first place.
func TestOnConflictUniqueIndexArbiter(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE t (a INT, b INT)`,
		`CREATE UNIQUE INDEX u_a ON t (a)`,
		`INSERT INTO t VALUES (1, 1)`,
		`INSERT INTO t VALUES (1, 2) ON CONFLICT (a) DO UPDATE SET b = excluded.b`,
	)
	if got := rowsOf(t, db, `SELECT a, b FROM t`); !slices.Equal(got, []string{"1|2"}) {
		t.Errorf("got %v, want [1|2]", got)
	}
}

func TestOnConflictErrors(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE t (a INT PRIMARY KEY, b INT)`,
	)

	tests := []struct {
		name string
		stmt string
		want string
	}{
		{
			// A target nothing enforces would never detect a collision, so the
			// statement would quietly behave like a plain INSERT.
			name: "target without a constraint",
			stmt: `INSERT INTO t VALUES (1, 1) ON CONFLICT (b) DO NOTHING`,
			want: "no unique or exclusion constraint",
		},
		{
			// DO UPDATE has to know which row it is changing.
			name: "do update without a target",
			stmt: `INSERT INTO t VALUES (1, 1) ON CONFLICT DO UPDATE SET b = 1`,
			want: "requires a conflict target",
		},
		{
			name: "unknown target column",
			stmt: `INSERT INTO t VALUES (1, 1) ON CONFLICT (nope) DO NOTHING`,
			want: "does not exist",
		},
		{
			name: "unknown assignment column",
			stmt: `INSERT INTO t VALUES (1, 1) ON CONFLICT (a) DO UPDATE SET nope = 1`,
			want: "does not exist",
		},
		{
			name: "repeated assignment",
			stmt: `INSERT INTO t VALUES (1, 1) ON CONFLICT (a) DO UPDATE SET b = 1, b = 2`,
			want: "multiple assignments",
		},
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
