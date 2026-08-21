package driver_test

import (
	"slices"
	"strings"
	"testing"
)

func TestCreateIndex(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, `CREATE TABLE t (a INT, b INT, s TEXT)`)

	// A plain index changes nothing a query can observe -- there is no index
	// selection in the planner -- so the assertion is that it is accepted and
	// that queries keep working, not that anything got faster.
	mustExecAll(t, db,
		`CREATE INDEX i_a ON t (a)`,
		`CREATE INDEX i_ab ON t (a, b)`,
		`INSERT INTO t VALUES (1, 1, 'x'), (1, 2, 'y')`,
	)
	if got := rowsOf(t, db, `SELECT s FROM t WHERE a = 1 ORDER BY b`); !slices.Equal(got, []string{"x", "y"}) {
		t.Errorf("got %v", got)
	}

	// Index names share one namespace per schema, so a repeat is a conflict.
	if err := queryErr(db, `CREATE INDEX i_a ON t (b)`); err == nil {
		t.Error("expected a duplicate index name to be rejected")
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS i_a ON t (b)`); err != nil {
		t.Errorf("IF NOT EXISTS should tolerate an existing index: %v", err)
	}

	if err := queryErr(db, `CREATE INDEX bad ON t (nosuch)`); err == nil {
		t.Error("expected an unknown column to be rejected")
	}
	if err := queryErr(db, `CREATE INDEX bad ON nosuchtable (a)`); err == nil {
		t.Error("expected an unknown table to be rejected")
	}
	if err := queryErr(db, `CREATE INDEX bad ON t (a, a)`); err == nil {
		t.Error("expected a repeated column to be rejected")
	}
}

// TestUniqueIndex covers the half of CREATE INDEX that is not advisory: a
// UNIQUE index is a constraint, and ignoring it would let duplicates through
// silently.
func TestUniqueIndex(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE t (a INT, b INT)`,
		`CREATE UNIQUE INDEX u_a ON t (a)`,
		`INSERT INTO t VALUES (1, 1)`,
	)

	err := queryErr(db, `INSERT INTO t VALUES (1, 2)`)
	if err == nil {
		t.Fatal("expected the unique index to reject a duplicate")
	}
	if !strings.Contains(err.Error(), "u_a") {
		t.Errorf("got %v, want it to name the index", err)
	}

	// An UPDATE that creates a duplicate is caught too, since uniqueness is
	// verified over the whole table at the end of the statement.
	mustExecAll(t, db, `INSERT INTO t VALUES (2, 2)`)
	if err := queryErr(db, `UPDATE t SET a = 1 WHERE a = 2`); err == nil {
		t.Error("expected the update to be rejected")
	}

	// NULLs are exempt, as they are for a UNIQUE constraint: a NULL is never
	// equal to anything, including another NULL.
	mustExecAll(t, db, `INSERT INTO t VALUES (NULL, 3)`, `INSERT INTO t VALUES (NULL, 4)`)
	if got := rowsOf(t, db, `SELECT count(*) FROM t WHERE a IS NULL`); !slices.Equal(got, []string{"2"}) {
		t.Errorf("two NULL keys should coexist, got %v", got)
	}
}

// TestPartialUniqueIndex is the case that makes indexes more than bookkeeping.
// The predicate decides which rows the index covers, so the same table can
// forbid a duplicate in one subset and permit it in another.
func TestPartialUniqueIndex(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE ids (security_id TEXT, kind TEXT, value TEXT)`,
		`CREATE UNIQUE INDEX idx_ids ON ids (kind, value) WHERE kind IN ('ISIN', 'WKN')`,
		`INSERT INTO ids VALUES ('a', 'ISIN', 'X')`,
	)

	// Covered by the predicate, so the duplicate is refused.
	if err := queryErr(db, `INSERT INTO ids VALUES ('b', 'ISIN', 'X')`); err == nil {
		t.Error("expected a duplicate ISIN to be rejected")
	}

	// Outside the predicate, so duplicates are fine. This is the half that a
	// unique index ignoring its WHERE clause would get wrong.
	mustExecAll(t, db,
		`INSERT INTO ids VALUES ('c', 'TICKER', 'Y')`,
		`INSERT INTO ids VALUES ('d', 'TICKER', 'Y')`,
	)
	if got := rowsOf(t, db, `SELECT count(*) FROM ids WHERE kind = 'TICKER'`); !slices.Equal(got, []string{"2"}) {
		t.Errorf("duplicate tickers should be allowed, got %v", got)
	}

	// Dropping the index removes the constraint with it.
	mustExecAll(t, db, `DROP INDEX idx_ids`, `INSERT INTO ids VALUES ('e', 'ISIN', 'X')`)
	if got := rowsOf(t, db, `SELECT count(*) FROM ids WHERE kind = 'ISIN'`); !slices.Equal(got, []string{"2"}) {
		t.Errorf("after dropping the index the duplicate should be allowed, got %v", got)
	}
}

func TestDropIndex(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, `CREATE TABLE t (a INT)`, `CREATE INDEX i ON t (a)`)

	if err := queryErr(db, `DROP INDEX nope`); err == nil {
		t.Error("expected an error dropping an index that does not exist")
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS nope`); err != nil {
		t.Errorf("IF EXISTS should tolerate a missing index: %v", err)
	}
	if _, err := db.Exec(`DROP INDEX i`); err != nil {
		t.Fatal(err)
	}
	// The name is free again.
	if _, err := db.Exec(`CREATE INDEX i ON t (a)`); err != nil {
		t.Errorf("the name should be reusable after the drop: %v", err)
	}
}

// TestIndexPredicateIsChecked confirms a bad partial predicate is rejected when
// the index is created, not at the first insert that would have consulted it.
func TestIndexPredicateIsChecked(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, `CREATE TABLE t (a INT)`)

	if err := queryErr(db, `CREATE UNIQUE INDEX i ON t (a) WHERE nosuch = 1`); err == nil {
		t.Error("expected an unknown column in the predicate to be rejected")
	}
	// The predicate must be a predicate, not just any expression.
	if err := queryErr(db, `CREATE UNIQUE INDEX i ON t (a) WHERE a`); err == nil {
		t.Error("expected a non-boolean index predicate to be rejected")
	}
}
