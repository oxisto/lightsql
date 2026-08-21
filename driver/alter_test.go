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
		// The forms a reader is most likely to try are named rather than left
		// to a bare syntax error, and the message says why.
		{"add column", `ALTER TABLE t ADD COLUMN c INT`, "rewrite every stored row"},
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
