package driver_test

import (
	"slices"
	"strings"
	"testing"
)

func TestAlterTableRename(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE old (id INT, nm TEXT)`,
		`INSERT INTO old VALUES (1, 'x')`,
	)

	if _, err := db.Exec(`ALTER TABLE old RENAME TO renamed`); err != nil {
		t.Fatal(err)
	}
	// The rows come with it; only the name changed.
	if got := rowsOf(t, db, `SELECT id, nm FROM renamed`); !slices.Equal(got, []string{"1|x"}) {
		t.Errorf("got %v, want [1|x]", got)
	}
	if err := queryErr(db, `SELECT * FROM old`); err == nil {
		t.Error("the old name still resolves")
	}
	// The old name is free for reuse.
	if _, err := db.Exec(`CREATE TABLE old (other INT)`); err != nil {
		t.Errorf("the old name should be reusable: %v", err)
	}
}

func TestAlterTableRenameColumn(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE t (id INT, nm TEXT)`,
		`INSERT INTO t VALUES (1, 'x')`,
		`ALTER TABLE t RENAME COLUMN nm TO name`,
	)

	if got := rowsOf(t, db, `SELECT name FROM t`); !slices.Equal(got, []string{"x"}) {
		t.Errorf("got %v, want [x]", got)
	}
	if err := queryErr(db, `SELECT nm FROM t`); err == nil {
		t.Error("the old column name still resolves")
	}
	// SELECT * reports the new name.
	rows, err := db.Query(`SELECT * FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	cols, err := rows.Columns()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"id", "name"}; !slices.Equal(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}

	// The COLUMN keyword is optional, as in PostgreSQL.
	if _, err := db.Exec(`ALTER TABLE t RENAME name TO label`); err != nil {
		t.Errorf("RENAME without COLUMN should work: %v", err)
	}
	if got := rowsOf(t, db, `SELECT label FROM t`); !slices.Equal(got, []string{"x"}) {
		t.Errorf("got %v", got)
	}
}

// TestAlterTableRenameKeepsReferences pins the payoff of resolving names once:
// a foreign key holds a table pointer and column ordinals, so renaming either
// end leaves the constraint working without being rewritten.
func TestAlterTableRenameKeepsReferences(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE parent (id INT PRIMARY KEY)`,
		`CREATE TABLE child (p INT REFERENCES parent(id))`,
		`INSERT INTO parent VALUES (1)`,
		`INSERT INTO child VALUES (1)`,
		`ALTER TABLE parent RENAME TO p2`,
		`ALTER TABLE p2 RENAME COLUMN id TO ident`,
	)

	// Still enforced after both renames.
	if err := queryErr(db, `INSERT INTO child VALUES (999)`); err == nil {
		t.Error("the foreign key stopped being enforced after the rename")
	}
	if _, err := db.Exec(`INSERT INTO child VALUES (1)`); err != nil {
		t.Errorf("a valid reference should still be accepted: %v", err)
	}
	// And the parent is reachable under its new names.
	if got := rowsOf(t, db, `SELECT ident FROM p2`); !slices.Equal(got, []string{"1"}) {
		t.Errorf("got %v", got)
	}
}

func TestAlterTableErrors(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE t (id INT, nm TEXT)`,
		`CREATE TABLE taken (id INT)`,
	)

	tests := []struct {
		name string
		stmt string
		want string
	}{
		{"unknown table", `ALTER TABLE nope RENAME TO x`, "does not exist"},
		{"name already taken", `ALTER TABLE t RENAME TO taken`, "already exists"},
		{"unknown column", `ALTER TABLE t RENAME COLUMN nope TO x`, "does not exist"},
		{"column name taken", `ALTER TABLE t RENAME COLUMN nm TO id`, "already exists"},
		// DROP COLUMN is named rather than left to a bare syntax error: unlike
		// ADD COLUMN it cannot be served by a missing value, since dropping
		// shifts every later column's ordinal.
		{"drop column", `ALTER TABLE t DROP COLUMN nm`, "rewrite every stored row"},
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

// TestAlterTableRenameColumnDependencies pins a refusal rather than a feature.
//
// A CHECK predicate, a DEFAULT and a partial index predicate are stored as
// syntax, so they name a column rather than addressing it by ordinal. Renaming
// without rewriting them leaves the table un-insertable, failing on a column the
// schema no longer shows -- which is worse than refusing the rename, and is what
// happened before this check existed.
func TestAlterTableRenameColumnDependencies(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE r (kind TEXT, n INT CHECK (n > 0), d INT DEFAULT 0)`,
		`CREATE UNIQUE INDEX ix ON r (kind) WHERE kind = 'a'`,
	)

	for _, tt := range []struct{ name, stmt, want string }{
		{"partial index predicate", `ALTER TABLE r RENAME COLUMN kind TO sort`, `index "ix"`},
		{"check constraint", `ALTER TABLE r RENAME COLUMN n TO m`, "check constraint"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := queryErr(db, tt.stmt)
			if err == nil {
				t.Fatalf("%s: expected the rename to be refused", tt.stmt)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want it to name what depends on the column", err)
			}
		})
	}

	// A column nothing refers to by name still renames, so the check is not a
	// blanket refusal.
	if _, err := db.Exec(`ALTER TABLE r RENAME COLUMN d TO e`); err != nil {
		t.Errorf("a column with no dependants should rename: %v", err)
	}
	// And the table still works afterwards.
	if _, err := db.Exec(`INSERT INTO r (kind, n) VALUES ('a', 1)`); err != nil {
		t.Errorf("insert after a permitted rename: %v", err)
	}
}

// TestAlterTableAddColumn covers the half of ADD COLUMN that is not syntax: the
// rows already stored are not rewritten, so they are shorter than the table and
// read the new column as its missing value.
func TestAlterTableAddColumn(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE t (id INT, nm TEXT)`,
		`INSERT INTO t VALUES (1, 'a'), (2, 'b')`,
		`ALTER TABLE t ADD COLUMN extra INT`,
		`ALTER TABLE t ADD COLUMN tag TEXT DEFAULT 'none'`,
	)

	// A row written before the column existed reads NULL, or the DEFAULT when
	// one was given.
	got := rowsOf(t, db, `SELECT id, nm, extra, tag FROM t ORDER BY id`)
	if want := []string{"1|a|NULL|none", "2|b|NULL|none"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// The new column behaves like any other afterwards: it can be written,
	// filtered on and aggregated.
	mustExecAll(t, db, `UPDATE t SET extra = 9 WHERE id = 1`)
	if got := rowsOf(t, db, `SELECT id FROM t WHERE extra = 9`); !slices.Equal(got, []string{"1"}) {
		t.Errorf("filter on the new column: got %v", got)
	}
	if got := rowsOf(t, db, `SELECT sum(extra) FROM t`); !slices.Equal(got, []string{"9"}) {
		t.Errorf("aggregate over the new column: got %v", got)
	}
	if got := rowsOf(t, db, `SELECT count(*) FROM t WHERE tag = 'none'`); !slices.Equal(got, []string{"2"}) {
		t.Errorf("filter on the defaulted column: got %v", got)
	}

	// A row inserted afterwards is stored at the full width and agrees with the
	// short ones.
	mustExecAll(t, db, `INSERT INTO t (id, nm) VALUES (3, 'c')`)
	got = rowsOf(t, db, `SELECT id, tag FROM t ORDER BY id`)
	if want := []string{"1|none", "2|none", "3|none"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// SELECT * reports the new columns.
	rows, err := db.Query(`SELECT * FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	cols, err := rows.Columns()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"id", "nm", "extra", "tag"}; !slices.Equal(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
}

// TestAlterTableAddColumnReferences is the statement money-gopher's migration
// needs, and the foreign key has to be enforced rather than merely recorded.
func TestAlterTableAddColumnReferences(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE persons (id TEXT PRIMARY KEY)`,
		`CREATE TABLE sessions (id TEXT PRIMARY KEY)`,
		`INSERT INTO persons VALUES ('p1')`,
		`INSERT INTO sessions VALUES ('s1')`,
		`ALTER TABLE sessions ADD COLUMN person_id TEXT REFERENCES persons(id) ON DELETE SET NULL`,
	)

	if err := queryErr(db, `INSERT INTO sessions VALUES ('s2', 'nope')`); err == nil {
		t.Error("a dangling reference was accepted; the foreign key is recorded but not enforced")
	}
	mustExecAll(t, db, `INSERT INTO sessions VALUES ('s3', 'p1')`)

	// The referential action fires too, which means the parent learned about
	// the new child rather than only the child knowing about the parent.
	mustExecAll(t, db, `DELETE FROM persons WHERE id = 'p1'`)
	got := rowsOf(t, db, `SELECT id, person_id FROM sessions ORDER BY id`)
	if want := []string{"s1|NULL", "s3|NULL"}; !slices.Equal(got, want) {
		t.Errorf("after ON DELETE SET NULL: got %v, want %v", got, want)
	}
}

func TestAlterTableAddColumnErrors(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE t (id INT)`,
		`INSERT INTO t VALUES (1)`,
		`ALTER TABLE t ADD COLUMN dup INT`,
	)

	if err := queryErr(db, `ALTER TABLE t ADD COLUMN dup INT`); err == nil {
		t.Error("expected a duplicate column to be rejected")
	}
	if _, err := db.Exec(`ALTER TABLE t ADD COLUMN IF NOT EXISTS dup INT`); err != nil {
		t.Errorf("IF NOT EXISTS should tolerate an existing column: %v", err)
	}

	// Every stored row would violate it, since they all read the column as
	// NULL. Refusing beats writing a table that cannot be read back.
	err := queryErr(db, `ALTER TABLE t ADD COLUMN n INT NOT NULL`)
	if err == nil {
		t.Error("expected NOT NULL without a default to be rejected")
	} else if !strings.Contains(err.Error(), "null values") {
		t.Errorf("got %v", err)
	}
	// With a default there is a value for them to take.
	if _, err := db.Exec(`ALTER TABLE t ADD COLUMN n INT NOT NULL DEFAULT 0`); err != nil {
		t.Errorf("NOT NULL with a default should be accepted: %v", err)
	}

	// A key over a column every existing row reads identically either collides
	// immediately or belongs in CREATE TABLE.
	if err := queryErr(db, `ALTER TABLE t ADD COLUMN k INT PRIMARY KEY`); err == nil {
		t.Error("expected PRIMARY KEY on ADD COLUMN to be rejected")
	}
}
