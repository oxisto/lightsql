package storage

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/types"
)

// row is a one-column row, enough for every test here.
func row(n int64) []types.Value { return []types.Value{types.Int(n)} }

func vals(vs []*Version) []int64 {
	out := make([]int64, len(vs))
	for i, v := range vs {
		out[i] = v.Vals[0].AsInt()
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

// TestVisibilityMatrix enumerates every combination of creating and deleting
// transaction state against a reader, rather than spot-checking a few. Both
// halves of the rule are easy to get backwards in a way that leaves a database
// which mostly works, so "mostly" is not a useful level of confidence here.
func TestVisibilityMatrix(t *testing.T) {
	// Each case builds a fresh manager so the ids line up predictably.
	type state string
	const (
		none      state = "none"
		committed state = "committed"
		aborted   state = "aborted"
		running   state = "running"
		self      state = "self"
	)

	tests := []struct {
		creator, deleter state
		want             bool
		why              string
	}{
		{creator: committed, deleter: none, want: true, why: "a committed insert with no delete is the ordinary visible row"},
		{creator: committed, deleter: committed, want: false, why: "deleted by a transaction the reader can see"},
		{creator: committed, deleter: aborted, want: true, why: "the deleting transaction rolled back, so the delete never happened"},
		{creator: committed, deleter: running, want: true, why: "the delete is not committed yet, so it is invisible"},
		{creator: committed, deleter: self, want: false, why: "a transaction does not see rows it deleted itself"},

		{creator: aborted, deleter: none, want: false, why: "the insert rolled back"},
		{creator: running, deleter: none, want: false, why: "another transaction's uncommitted insert"},
		{creator: self, deleter: none, want: true, why: "a transaction always sees its own writes"},
		{creator: self, deleter: self, want: false, why: "inserted and then deleted within one transaction"},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("created_%s/deleted_%s", tt.creator, tt.deleter)
		t.Run(name, func(t *testing.T) {
			m := NewTxManager()

			// The reader is created first so that "running" transactions below
			// are genuinely concurrent with it.
			reader := m.Begin()

			mk := func(s state) TxID {
				switch s {
				case none:
					return InvalidTxID
				case self:
					return reader
				case committed:
					id := m.Begin()
					m.Commit(id)
					return id
				case aborted:
					id := m.Begin()
					m.Abort(id)
					return id
				default: // running
					return m.Begin()
				}
			}

			v := &Version{XMin: mk(tt.creator), XMax: mk(tt.deleter)}
			// The snapshot is taken after the other transactions exist, so a
			// committed one is genuinely visible and a running one is not.
			snap := m.Take()

			if got := v.Visible(snap, reader); got != tt.want {
				t.Errorf("Visible = %v, want %v: %s", got, tt.want, tt.why)
			}
		})
	}
}

// TestSnapshotIsStable is the property that makes a repeatable read repeatable:
// a transaction that commits after the snapshot was taken stays invisible, even
// though it is committed by the time the row is examined.
func TestSnapshotIsStable(t *testing.T) {
	m := NewTxManager()
	h := NewHeap(m)

	writer := m.Begin()
	h.Insert(writer, row(1))

	reader := m.Begin()
	snap := m.Take() // taken while the writer is still running

	// The writer commits after the snapshot exists.
	m.Commit(writer)

	if got := h.Scan(snap, reader); len(got) != 0 {
		t.Errorf("scan saw %v; a transaction that committed after the snapshot must stay invisible", vals(got))
	}
	// A fresh snapshot does see it, which is what READ COMMITTED does per
	// statement.
	if got := h.Scan(m.Take(), reader); !equal(vals(got), []int64{1}) {
		t.Errorf("a new snapshot saw %v, want [1]", vals(got))
	}
}

func TestOwnWritesAreVisible(t *testing.T) {
	m := NewTxManager()
	h := NewHeap(m)

	tx := m.Begin()
	h.Insert(tx, row(1))
	snap := m.Take()

	// Uncommitted, but its own author sees it.
	if got := h.Scan(snap, tx); !equal(vals(got), []int64{1}) {
		t.Errorf("a transaction saw %v of its own writes, want [1]", vals(got))
	}
	// Nobody else does.
	other := m.Begin()
	if got := h.Scan(m.Take(), other); len(got) != 0 {
		t.Errorf("another transaction saw %v, want nothing", vals(got))
	}
}

// TestAbortHidesEverything is the whole of rollback: no undo log is replayed, so
// there is no way for a rollback to be partially applied.
func TestAbortHidesEverything(t *testing.T) {
	m := NewTxManager()
	h := NewHeap(m)

	setup := m.Begin()
	v := h.Insert(setup, row(1))
	m.Commit(setup)

	tx := m.Begin()
	h.Insert(tx, row(2))
	h.Insert(tx, row(3))
	if err := h.Delete(tx, v); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	m.Abort(tx)

	// The inserts vanish and the delete is undone, in one step.
	reader := m.Begin()
	if got := h.Scan(m.Take(), reader); !equal(vals(got), []int64{1}) {
		t.Errorf("after abort the table shows %v, want [1]", vals(got))
	}
}

func TestWriteConflict(t *testing.T) {
	m := NewTxManager()
	h := NewHeap(m)

	setup := m.Begin()
	v := h.Insert(setup, row(1))
	m.Commit(setup)

	a, b := m.Begin(), m.Begin()
	if err := h.Delete(a, v); err != nil {
		t.Fatalf("first delete: %v", err)
	}

	// The second transaction cannot silently overwrite the first.
	err := h.Delete(b, v)
	if !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("second delete gave %v, want a write conflict", err)
	}
	var e *pgerr.Error
	if !errors.As(err, &e) || e.Code != pgerr.SerializationFailure {
		t.Errorf("conflict error %v does not carry SQLSTATE 40001", err)
	}

	// Once the first transaction rolls back, the row is free again.
	m.Abort(a)
	if err := h.Delete(b, v); err != nil {
		t.Errorf("after the conflicting transaction aborted: %v", err)
	}
}

// TestDeleteIsIdempotentWithinATransaction checks that a statement touching the
// same row twice is not a conflict with itself.
func TestDeleteIsIdempotentWithinATransaction(t *testing.T) {
	m := NewTxManager()
	h := NewHeap(m)

	setup := m.Begin()
	v := h.Insert(setup, row(1))
	m.Commit(setup)

	tx := m.Begin()
	if err := h.Delete(tx, v); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := h.Delete(tx, v); err != nil {
		t.Errorf("deleting the same row twice in one transaction: %v", err)
	}
}

func TestVacuum(t *testing.T) {
	m := NewTxManager()
	h := NewHeap(m)

	// A committed live row, a committed deleted row, and an aborted insert.
	keep := m.Begin()
	h.Insert(keep, row(1))
	dead := h.Insert(keep, row(2))
	m.Commit(keep)

	del := m.Begin()
	if err := h.Delete(del, dead); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	m.Commit(del)

	rolled := m.Begin()
	h.Insert(rolled, row(3))
	m.Abort(rolled)

	if removed := h.Vacuum(m.Horizon()); removed != 2 {
		t.Errorf("Vacuum removed %d versions, want 2", removed)
	}
	reader := m.Begin()
	if got := h.Scan(m.Take(), reader); !equal(vals(got), []int64{1}) {
		t.Errorf("after vacuum the table shows %v, want [1]", vals(got))
	}
}

// TestVacuumKeepsVersionsAnOldSnapshotNeeds pins the reason Vacuum takes a
// horizon rather than discarding everything deleted: a transaction that started
// earlier may still be reading a version that is dead to newer ones.
func TestVacuumKeepsVersionsAnOldSnapshotNeeds(t *testing.T) {
	m := NewTxManager()
	h := NewHeap(m)

	setup := m.Begin()
	v := h.Insert(setup, row(1))
	m.Commit(setup)

	// An old reader takes its snapshot before the delete.
	reader := m.Begin()
	snap := m.Take()

	del := m.Begin()
	if err := h.Delete(del, v); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	m.Commit(del)

	// Vacuuming only up to the old reader must not discard what it can see.
	h.Vacuum(reader)
	if got := h.Scan(snap, reader); !equal(vals(got), []int64{1}) {
		t.Errorf("the old snapshot lost its row: saw %v, want [1]", vals(got))
	}
}

// TestSnapshotAnswersDoNotChange is the regression test for allocating a
// transaction id outside the manager's lock.
//
// A snapshot must give the same answer about a transaction forever. If the id
// counter advances before the transaction is registered as active, a snapshot
// can be taken whose XMax is already past it while it is missing from the
// exclusion set — so it reads as finished-but-not-excluded, and becomes visible
// the moment it commits. The race detector does not catch this: every
// individual access is properly synchronised, and only the combination is wrong.
//
// The check is therefore behavioural: record what a snapshot says while writers
// are running, then ask it again once everything has committed.
func TestSnapshotAnswersDoNotChange(t *testing.T) {
	const rounds = 200

	for range rounds {
		m := NewTxManager()

		var wg sync.WaitGroup
		start := make(chan struct{})

		// Writers begin transactions concurrently with the snapshot below.
		ids := make([]TxID, 4)
		for i := range ids {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				ids[i] = m.Begin()
			}(i)
		}

		var snap *Snapshot
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			snap = m.Take()
		}()

		close(start)
		wg.Wait()

		// What the snapshot says now, while nothing has committed.
		before := make([]bool, len(ids))
		for i, id := range ids {
			before[i] = snap.committed(id)
		}
		// None of them can be visible: they were either not yet issued when the
		// snapshot was taken, or in progress.
		for i, seen := range before {
			if seen {
				t.Fatalf("snapshot saw transaction %d before it committed", ids[i])
			}
		}

		for _, id := range ids {
			m.Commit(id)
		}

		// The same snapshot must still say the same thing.
		for i, id := range ids {
			if snap.committed(id) != before[i] {
				t.Fatalf("snapshot changed its answer about transaction %d after it "+
					"committed: the id was allocated outside the manager's lock", id)
			}
		}
	}
}

// TestFinishIgnoresUnissuedIDs pins that a transaction id nobody handed out
// cannot be conjured into existence by committing it. Recording a status for an
// unknown id would contradict Status reporting unknown ids as aborted, and could
// make rows referring to that id visible.
func TestFinishIgnoresUnissuedIDs(t *testing.T) {
	m := NewTxManager()

	m.Commit(TxID(42))
	if got := m.Status(TxID(42)); got != Aborted {
		t.Errorf("after committing an unissued id, Status = %s, want aborted", got)
	}
	m.Abort(TxID(43))
	if got := m.Status(TxID(43)); got != Aborted {
		t.Errorf("after aborting an unissued id, Status = %s, want aborted", got)
	}

	// A row created by an unissued transaction stays invisible.
	reader := m.Begin()
	v := &Version{XMin: TxID(42)}
	if v.Visible(m.Take(), reader) {
		t.Error("a row created by an unissued transaction is visible")
	}
}

func TestStatusOfUnknownTransaction(t *testing.T) {
	m := NewTxManager()
	// An id that was never issued must read as aborted, so a row referring to
	// one is invisible rather than trusted.
	if got := m.Status(TxID(999)); got != Aborted {
		t.Errorf("Status of an unissued id = %s, want aborted", got)
	}
}

func TestFinishIsIdempotent(t *testing.T) {
	m := NewTxManager()

	tx := m.Begin()
	m.Commit(tx)
	// A deferred Rollback after a Commit is the usual shape of correct caller
	// code, so it must not undo the commit.
	m.Abort(tx)
	if got := m.Status(tx); got != Committed {
		t.Errorf("status after Commit then Abort = %s, want committed", got)
	}
}

// TestConcurrentReadersAndWriters is the race-detector case: many transactions
// reading and writing one heap at once, which is the normal situation when
// database/sql hands one engine to a pool of connections.
func TestConcurrentReadersAndWriters(t *testing.T) {
	m := NewTxManager()
	h := NewHeap(m)

	const workers = 8
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(n int64) {
			defer wg.Done()
			for range 50 {
				tx := m.Begin()
				h.Insert(tx, row(n))
				h.Scan(m.Take(), tx)
				if n%2 == 0 {
					m.Commit(tx)
				} else {
					m.Abort(tx)
				}
			}
		}(int64(i))
	}
	wg.Wait()

	// Every committed insert is visible and no aborted one is.
	reader := m.Begin()
	got := h.Scan(m.Take(), reader)
	if len(got) != workers/2*50 {
		t.Errorf("saw %d rows, want %d", len(got), workers/2*50)
	}
	for _, v := range got {
		if v.Vals[0].AsInt()%2 != 0 {
			t.Fatalf("saw a row from an aborted transaction: %v", v.Vals[0])
		}
	}
}
