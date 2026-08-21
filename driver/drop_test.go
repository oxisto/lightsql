package driver_test

import (
	"slices"
	"strings"
	"testing"
)

func TestDropTable(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE keep (id INT)`,
		`CREATE TABLE gone (id INT)`,
		`INSERT INTO keep VALUES (1)`,
		`INSERT INTO gone VALUES (1)`,
	)

	if _, err := db.Exec(`DROP TABLE gone`); err != nil {
		t.Fatal(err)
	}
	if err := queryErr(db, `SELECT * FROM gone`); err == nil {
		t.Error("the table is still queryable after being dropped")
	}
	// Dropping one table must not disturb another.
	if got := rowsOf(t, db, `SELECT id FROM keep`); !slices.Equal(got, []string{"1"}) {
		t.Errorf("keep = %v, want [1]", got)
	}

	// The name is free again afterwards, and the new table starts empty rather
	// than inheriting the old rows.
	mustExecAll(t, db, `CREATE TABLE gone (id INT, extra TEXT)`)
	if got := rowsOf(t, db, `SELECT count(*) FROM gone`); !slices.Equal(got, []string{"0"}) {
		t.Errorf("recreated table has %v rows, want 0", got)
	}
}

func TestDropTableIfExists(t *testing.T) {
	db := open(t)

	if err := queryErr(db, `DROP TABLE nope`); err == nil {
		t.Error("expected an error dropping a table that does not exist")
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS nope`); err != nil {
		t.Errorf("IF EXISTS should tolerate a missing table: %v", err)
	}

	// IF EXISTS applies to the whole list, so a mix of present and absent is
	// not an error and the present one is still dropped.
	mustExecAll(t, db, `CREATE TABLE here (id INT)`)
	if _, err := db.Exec(`DROP TABLE IF EXISTS here, nope`); err != nil {
		t.Fatal(err)
	}
	if err := queryErr(db, `SELECT * FROM here`); err == nil {
		t.Error("here survived a DROP that reported success")
	}
}

func TestDropTableSeveral(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE a (id INT)`,
		`CREATE TABLE b (id INT)`,
		`CREATE TABLE c (id INT)`,
	)
	if _, err := db.Exec(`DROP TABLE a, b`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SELECT * FROM a`, `SELECT * FROM b`} {
		if err := queryErr(db, q); err == nil {
			t.Errorf("%s: table survived the drop", q)
		}
	}
	if err := queryErr(db, `SELECT * FROM c`); err != nil {
		t.Errorf("c should be untouched: %v", err)
	}

	// A name repeated in one statement is rejected, rather than the second
	// mention reporting a table that is missing only because this statement
	// removed it.
	if err := queryErr(db, `DROP TABLE c, c`); err == nil {
		t.Error("expected a repeated table name to be rejected")
	}
}

// TestDropTableReferences covers the rule that makes DROP TABLE more than a map
// delete: a table another one points at cannot go, and one that points at
// others must unregister itself on the way out.
func TestDropTableReferences(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE parent (id INT PRIMARY KEY)`,
		`CREATE TABLE child (id INT, p INT REFERENCES parent(id))`,
		`INSERT INTO parent VALUES (1)`,
		`INSERT INTO child VALUES (1, 1)`,
	)

	// RESTRICT is the default: the parent stays while the child references it.
	err := queryErr(db, `DROP TABLE parent`)
	if err == nil {
		t.Fatal("expected the reference to prevent the drop")
	}
	if !strings.Contains(err.Error(), "depends on it") {
		t.Errorf("got %v, want it to name the dependent table", err)
	}

	// The child may go, and doing so must unregister it from the parent. If it
	// does not, the parent keeps enforcing against a table nobody can see, and
	// the delete below fails for a reason that cannot be inspected.
	if _, err := db.Exec(`DROP TABLE child`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM parent WHERE id = 1`); err != nil {
		t.Fatalf("deleting the parent after its only child was dropped: %v", err)
	}

	// Naming both at once is legal even though the parent alone would not be,
	// because the reference is inside the set being dropped.
	mustExecAll(t, db,
		`CREATE TABLE p2 (id INT PRIMARY KEY)`,
		`CREATE TABLE c2 (p INT REFERENCES p2(id))`,
	)
	if _, err := db.Exec(`DROP TABLE c2, p2`); err != nil {
		t.Errorf("dropping a parent together with its child: %v", err)
	}
}

// TestDropTableCascadeRejected pins that CASCADE is refused rather than
// half-performed. Dropping the dependent constraint means removing it from the
// child, and a child's constraints are aliased by the parent's referencing
// list, so removing one moves what those pointers address.
func TestDropTableCascadeRejected(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE parent (id INT PRIMARY KEY)`,
		`CREATE TABLE child (p INT REFERENCES parent(id))`,
	)

	err := queryErr(db, `DROP TABLE parent CASCADE`)
	if err == nil {
		t.Fatal("expected CASCADE to be refused")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("got %v, want a feature-not-supported error", err)
	}

	// RESTRICT is the default and may also be written out.
	if _, err := db.Exec(`DROP TABLE child RESTRICT`); err != nil {
		t.Errorf("explicit RESTRICT should parse: %v", err)
	}
}

// TestDropTableIsNotTransactional pins a divergence from PostgreSQL rather than
// a feature: the catalog is not versioned, so a rolled back DROP does not bring
// the table back.
//
// This is not new to DROP. A rolled back CREATE TABLE leaves its table behind
// for the same reason — the catalog is plain structs mutated in place, while
// only row storage is MVCC. The test exists so the limitation is asserted
// somewhere instead of being discovered by someone whose migration half
// applied.
func TestDropTableIsNotTransactional(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, `CREATE TABLE t (id INT)`, `INSERT INTO t VALUES (1)`)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DROP TABLE t`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	if err := queryErr(db, `SELECT id FROM t`); err == nil {
		t.Error("the table came back after rollback; if DDL became transactional, " +
			"update this test and the compatibility note rather than deleting it")
	}
}
