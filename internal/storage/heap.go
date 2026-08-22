package storage

import (
	"sync"

	"github.com/oxisto/lightsql/internal/types"
)

// RowID identifies a row within its table, for as long as the row exists.
//
// It is needed because the write-ahead log has to name the row a record deletes,
// and it cannot do that with a pointer or a slice index: a pointer means nothing
// after a restart, and an index shifts when Vacuum compacts the slice. The id is
// allocated once and never reused, so a log record always refers to the row it
// was written for.
type RowID uint64

// Version is one version of a row.
//
// A row is never overwritten in place. An update writes a new version and stamps
// the old one as deleted by the updating transaction, so a reader holding an
// older snapshot keeps seeing the version that was current when it started.
type Version struct {
	// ID identifies the row this version belongs to. An update allocates a new
	// id rather than keeping the old one, because the log records it as a delete
	// of one row and an insert of another -- which is exactly what the heap
	// does in memory.
	ID RowID
	// XMin is the transaction that created this version.
	XMin TxID
	// XMax is the transaction that deleted it, or InvalidTxID while it is live.
	// An update sets XMax on the old version and writes a new one.
	XMax TxID
	// Vals is the row itself. It is never mutated after the version is linked
	// into the heap, which is what lets a reader hold one without copying.
	Vals []types.Value
}

// Visible reports whether this version is the one a transaction should see.
//
// The rule has two halves, and both matter:
//
//   - The version must have been created by a transaction this snapshot can
//     see, or by the reading transaction itself. A transaction always sees its
//     own writes, even though it has not committed.
//   - It must not have been deleted by a transaction this snapshot can see, nor
//     by the reading transaction itself. A transaction does not see rows it has
//     already deleted.
//
// Getting either half backwards produces a database that mostly works, which is
// why the exhaustive table in the tests enumerates every combination rather than
// spot-checking a few.
func (v *Version) Visible(snap *Snapshot, self TxID) bool {
	created := v.XMin == self || snap.committed(v.XMin)
	if !created {
		return false
	}
	if v.XMax == InvalidTxID {
		return true
	}
	deleted := v.XMax == self || snap.committed(v.XMax)
	return !deleted
}

// Heap stores every version of every row of one table.
//
// Versions are kept in a flat slice in write order rather than in per-row
// chains. A scan therefore walks them all and filters by visibility, which is
// linear in the number of versions rather than live rows — acceptable at the
// scale lightsql targets, and the cost that Vacuum exists to bound.
type Heap struct {
	// mgr resolves transaction outcomes. A heap cannot decide visibility on its
	// own, so it holds the manager rather than being handed a status per call.
	mgr *TxManager

	mu       sync.RWMutex
	versions []*Version
	// nextRow is the id the next row written into this heap will take. It is
	// per heap rather than global so that a log record only has to be unique
	// within the table it names.
	nextRow RowID
}

// NewHeap returns an empty heap whose visibility is judged by mgr.
func NewHeap(mgr *TxManager) *Heap { return &Heap{mgr: mgr, nextRow: 1} }

// Insert adds a new row created by tx.
func (h *Heap) Insert(tx TxID, vals []types.Value) *Version {
	h.mu.Lock()
	defer h.mu.Unlock()

	v := &Version{ID: h.nextRow, XMin: tx, Vals: vals}
	h.nextRow++
	h.versions = append(h.versions, v)
	return v
}

// Load places a row that was read back from the log, with the id it had when it
// was written.
//
// It is not Insert with an extra argument, because the two have opposite
// obligations: Insert allocates an id and its caller logs the result, while Load
// takes the id as given and must not be logged at all -- recovery replays the
// log, and writing what it reads back into the same log would double it on every
// restart.
func (h *Heap) Load(tx TxID, id RowID, vals []types.Value) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.versions = append(h.versions, &Version{ID: id, XMin: tx, Vals: vals})
	if id >= h.nextRow {
		h.nextRow = id + 1
	}
}

// Delete marks a version deleted by tx.
//
// It reports a conflict when the version is already being deleted by a different
// transaction that has not finished. Two transactions updating the same row is
// the one case snapshot isolation cannot silently resolve: whichever arrives
// second must be told, rather than quietly overwriting.
func (h *Heap) Delete(tx TxID, v *Version) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if v.XMax != InvalidTxID && v.XMax != tx {
		if h.statusOf(v.XMax) == InProgress {
			return ErrWriteConflict
		}
		if h.statusOf(v.XMax) == Committed {
			// Someone else already deleted it and committed, so this
			// transaction is working from a stale view of the row.
			return ErrWriteConflict
		}
		// The other transaction aborted, so its deletion never happened and the
		// version is free to take.
	}
	v.XMax = tx
	return nil
}

func (h *Heap) statusOf(id TxID) Status { return h.mgr.Status(id) }

// Scan returns the versions visible to a transaction under a snapshot.
//
// The returned slice is freshly allocated, so a caller may hold it while other
// transactions write. The versions inside are shared and must not be modified.
func (h *Heap) Scan(snap *Snapshot, self TxID) []*Version {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var out []*Version
	for _, v := range h.versions {
		if v.Visible(snap, self) {
			out = append(out, v)
		}
	}
	return out
}

// Vacuum discards versions that no snapshot can ever see again: those deleted by
// a committed transaction below the horizon, and those created by a transaction
// that aborted.
//
// It reports how many versions were removed. Without it, a table that is updated
// repeatedly grows without bound even when its live row count does not.
func (h *Heap) Vacuum(horizon TxID) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	kept := h.versions[:0]
	removed := 0
	for _, v := range h.versions {
		if h.deadTo(v, horizon) {
			removed++
			continue
		}
		kept = append(kept, v)
	}
	// Clear the tail so the discarded versions can be collected.
	for i := len(kept); i < len(h.versions); i++ {
		h.versions[i] = nil
	}
	h.versions = kept
	return removed
}

func (h *Heap) deadTo(v *Version, horizon TxID) bool {
	if h.statusOf(v.XMin) == Aborted {
		return true
	}
	return v.XMax != InvalidTxID &&
		v.XMax < horizon &&
		h.statusOf(v.XMax) == Committed
}
