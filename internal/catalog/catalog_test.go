package catalog

import (
	"testing"

	"github.com/oxisto/lightsql/internal/storage"
	"github.com/oxisto/lightsql/internal/types"
)

// fixture returns a two-column table holding the given integer rows, already
// committed, plus the manager the caller needs to start further transactions.
func fixture(t *testing.T, values ...int64) (*Table, *storage.TxManager) {
	t.Helper()

	intType, err := ResolveType("bigint", nil)
	if err != nil {
		t.Fatalf("ResolveType: %v", err)
	}
	mgr := storage.NewTxManager()
	tbl := &Table{
		Name:    "t",
		Columns: []Column{{Name: "a", Type: intType}, {Name: "b", Type: intType}},
	}
	if _, err := New(mgr).CreateTable(tbl, false); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	tx := mgr.BeginTx(storage.ReadCommitted, false)
	for _, v := range values {
		if err := tbl.Insert(tx, []types.Value{types.Int(v), types.Int(v * 10)}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return tbl, mgr
}

func colA(rows [][]types.Value) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r[0].AsInt()
	}
	return out
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReaderKeepsItsSnapshot is what the previous hand-rolled stand-in could
// only approximate. A reader that took its snapshot before an update keeps
// seeing the values it started with, even after the writer commits.
func TestReaderKeepsItsSnapshot(t *testing.T) {
	tbl, mgr := fixture(t, 1, 2, 3)

	// The reader runs at REPEATABLE READ, so its snapshot is taken once and
	// kept for the whole transaction.
	reader := mgr.BeginTx(storage.RepeatableRead, false)
	before := colA(tbl.Rows(reader))

	writer := mgr.BeginTx(storage.ReadCommitted, false)
	for _, v := range tbl.Scan(writer) {
		if err := tbl.Update(writer, v, []types.Value{types.Int(99), v.Vals[1]}); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	if err := writer.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if got := colA(tbl.Rows(reader)); !equal(got, before) {
		t.Errorf("the reader's view changed under it: %v, want %v", got, before)
	}
	// A transaction started afterwards does see the new values.
	after := mgr.BeginTx(storage.ReadCommitted, false)
	if got := colA(tbl.Rows(after)); !equal(got, []int64{99, 99, 99}) {
		t.Errorf("a new transaction saw %v, want [99 99 99]", got)
	}
}

// TestRollbackDiscardsEverything is the property ramsql gets wrong: its undo log
// silently fails to roll back updates and deletes. Here rollback is one flag, so
// there is no partial application to get wrong.
func TestRollbackDiscardsEverything(t *testing.T) {
	tbl, mgr := fixture(t, 1, 2, 3)

	tx := mgr.BeginTx(storage.ReadCommitted, false)
	rows := tbl.Scan(tx)
	if err := tbl.Update(tx, rows[0], []types.Value{types.Int(99), types.Int(0)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := tbl.Delete(tx, rows[1]); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := tbl.Insert(tx, []types.Value{types.Int(4), types.Int(40)}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// The transaction sees its own work.
	if got := colA(tbl.Rows(tx)); !equal(got, []int64{3, 99, 4}) {
		t.Errorf("the writer saw %v of its own changes, want [3 99 4]", got)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	after := mgr.BeginTx(storage.ReadCommitted, false)
	if got := colA(tbl.Rows(after)); !equal(got, []int64{1, 2, 3}) {
		t.Errorf("after rollback the table shows %v, want [1 2 3]; an update or "+
			"delete was left applied", got)
	}
}

// TestUpdateRewritesTheRow records where an updated row ends up. A version is
// never edited in place, so the new one is appended and the row moves to the
// end. SQL guarantees no order without ORDER BY, and this states what actually
// happens rather than implying otherwise.
func TestUpdateRewritesTheRow(t *testing.T) {
	tbl, mgr := fixture(t, 1, 2, 3)

	tx := mgr.BeginTx(storage.ReadCommitted, false)
	rows := tbl.Scan(tx)
	if err := tbl.Update(tx, rows[1], []types.Value{types.Int(20), types.Int(200)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	after := mgr.BeginTx(storage.ReadCommitted, false)
	if got := colA(tbl.Rows(after)); !equal(got, []int64{1, 3, 20}) {
		t.Errorf("rows = %v, want [1 3 20]", got)
	}
}

func TestNotNullIsEnforcedOnUpdate(t *testing.T) {
	tbl, mgr := fixture(t, 1)
	tbl.Columns[1].NotNull = true

	tx := mgr.BeginTx(storage.ReadCommitted, false)
	v := tbl.Scan(tx)[0]
	if err := tbl.Update(tx, v, []types.Value{types.Int(1), types.Null()}); err == nil {
		t.Fatal("an update to NULL on a NOT NULL column was accepted")
	}
	// The row is untouched, because the check runs before anything is written.
	if got := colA(tbl.Rows(tx)); !equal(got, []int64{1}) {
		t.Errorf("rows = %v after a refused update, want [1]", got)
	}
}

// TestConcurrentTransactionsSeeConsistentViews is the race-detector case at the
// catalog level: pooled connections make concurrent statements normal here.
func TestConcurrentTransactionsSeeConsistentViews(t *testing.T) {
	tbl, mgr := fixture(t, 1, 2, 3)

	done := make(chan bool, 4)
	for range 4 {
		go func() {
			tx := mgr.BeginTx(storage.RepeatableRead, false)
			first := colA(tbl.Rows(tx))
			// Reading twice through one snapshot must give the same answer,
			// whatever else is happening.
			second := colA(tbl.Rows(tx))
			done <- equal(first, second)
		}()
	}
	// Meanwhile a writer keeps changing the table.
	go func() {
		for i := range 10 {
			tx := mgr.BeginTx(storage.ReadCommitted, false)
			_ = tbl.Insert(tx, []types.Value{types.Int(int64(100 + i)), types.Int(0)})
			_ = tx.Commit()
		}
	}()

	for range 4 {
		if !<-done {
			t.Error("a transaction saw its own view change between two reads")
		}
	}
}
