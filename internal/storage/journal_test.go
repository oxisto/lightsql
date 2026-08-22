package storage

import (
	"errors"
	"testing"

	"github.com/oxisto/lightsql/internal/types"
	"github.com/oxisto/lightsql/internal/wal"
)

// recorder is a journal that keeps what it was handed, and can be told to fail.
type recorder struct {
	txs  [][]wal.Record
	fail error
}

func (r *recorder) Write(recs []wal.Record) error {
	if r.fail != nil {
		return r.fail
	}
	r.txs = append(r.txs, append([]wal.Record(nil), recs...))
	return nil
}

// TestRowIDsAreUniqueAndStable pins the property the log depends on: an id
// names one row for as long as it exists and is never handed out again. An
// update allocates a new one, because the log describes it as a delete followed
// by an insert.
func TestRowIDsAreUniqueAndStable(t *testing.T) {
	mgr := NewTxManager()
	h := NewHeap(mgr)
	tx := mgr.BeginTx(ReadCommitted, false)

	seen := map[RowID]bool{}
	first := h.Insert(tx.ID, []types.Value{types.Int(1)})
	seen[first.ID] = true
	for i := range 10 {
		v := h.Insert(tx.ID, []types.Value{types.Int(int64(i))})
		if seen[v.ID] {
			t.Fatalf("row id %d was handed out twice", v.ID)
		}
		seen[v.ID] = true
	}

	// Deleting and vacuuming must not free the id for reuse: a log record
	// written before the vacuum still names it.
	if err := h.Delete(tx.ID, first); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	h.Vacuum(mgr.Horizon())

	next := mgr.BeginTx(ReadCommitted, false)
	if v := h.Insert(next.ID, []types.Value{types.Int(99)}); seen[v.ID] {
		t.Errorf("row id %d was reused after a vacuum", v.ID)
	}
}

// TestLoadContinuesTheIDSequence covers the state a database is in just after
// recovery: the ids came from the log, and the next row written must not
// collide with one of them.
func TestLoadContinuesTheIDSequence(t *testing.T) {
	mgr := NewTxManager()
	h := NewHeap(mgr)
	boot := mgr.BeginTx(ReadCommitted, false)
	for _, id := range []RowID{3, 7, 4} {
		h.Load(boot.ID, id, []types.Value{types.Int(int64(id))})
	}
	if err := boot.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	tx := mgr.BeginTx(ReadCommitted, false)
	if v := h.Insert(tx.ID, []types.Value{types.Int(0)}); v.ID != 8 {
		t.Errorf("first row after recovery took id %d, want 8", v.ID)
	}
	if got := len(h.Scan(tx.Snapshot(), tx.ID)); got != 4 {
		t.Errorf("scan saw %d rows, want 4", got)
	}
}

// TestCommitWritesOnceRolledBackWritesNothing is the pair that makes the log
// match memory: a rollback costs nothing on disk because nothing was written,
// and a commit writes its records exactly once.
func TestCommitWritesOnceRolledBackWritesNothing(t *testing.T) {
	mgr := NewTxManager()
	j := &recorder{}
	mgr.SetJournal(j)

	kept := mgr.BeginTx(ReadCommitted, false)
	kept.Log(wal.InsertRecord("public.t", 1, []types.Value{types.Int(1)}))
	kept.Log(wal.InsertRecord("public.t", 2, []types.Value{types.Int(2)}))
	if err := kept.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	gone := mgr.BeginTx(ReadCommitted, false)
	gone.Log(wal.InsertRecord("public.t", 3, []types.Value{types.Int(3)}))
	if err := gone.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if len(j.txs) != 1 {
		t.Fatalf("journal holds %d transactions, want 1", len(j.txs))
	}
	if len(j.txs[0]) != 2 {
		t.Errorf("the committed transaction wrote %d records, want 2", len(j.txs[0]))
	}
}

// TestCommitFailsWhenTheJournalDoes pins durable-before-visible. If the records
// cannot be written the transaction must not commit, or the database would go
// on serving rows that are not on disk and would lose them at the next restart
// without ever having reported a failure.
func TestCommitFailsWhenTheJournalDoes(t *testing.T) {
	mgr := NewTxManager()
	boom := errors.New("disk is full")
	mgr.SetJournal(&recorder{fail: boom})

	tx := mgr.BeginTx(ReadCommitted, false)
	tx.Log(wal.InsertRecord("public.t", 1, []types.Value{types.Int(1)}))

	if err := tx.Commit(); !errors.Is(err, boom) {
		t.Fatalf("Commit returned %v, want the journal's error", err)
	}
	if got := mgr.Status(tx.ID); got != Aborted {
		t.Errorf("after a failed commit the transaction is %s, want aborted", got)
	}
}

// TestNoJournalRecordsNothing covers the in-memory database, which is the
// default: the record path has to be inert rather than merely cheap, since
// every row written would otherwise be buffered until commit and never used.
func TestNoJournalRecordsNothing(t *testing.T) {
	mgr := NewTxManager()
	tx := mgr.BeginTx(ReadCommitted, false)
	tx.Log(wal.InsertRecord("public.t", 1, []types.Value{types.Int(1)}))
	if len(tx.pending) != 0 {
		t.Errorf("an unjournalled transaction buffered %d records", len(tx.pending))
	}
	if err := tx.LogNow(wal.DDLRecord("create table t (a int)")); err != nil {
		t.Errorf("LogNow without a journal: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Errorf("Commit: %v", err)
	}
}
