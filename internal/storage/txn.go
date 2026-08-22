// Package storage holds the row heap and the transaction manager.
//
// Rows are versioned rather than overwritten: an update writes a new version and
// marks the old one deleted, and each transaction reads through a snapshot that
// decides which versions it can see. That buys three things which are otherwise
// hard to get right at once:
//
//   - Rollback is marking a transaction aborted. There is no undo log to replay,
//     so there is no undo log to get wrong — which is exactly how a rollback
//     ends up silently applying half a statement.
//   - Readers never block writers. database/sql hands one engine to a pool of
//     connections, so concurrent statements are the normal case here, not an
//     edge case.
//   - The isolation levels of sql.TxOptions map onto snapshot rules directly,
//     instead of being accepted and ignored.
package storage

import (
	"sync"

	"github.com/oxisto/lightsql/internal/pgerr"
)

// TxID identifies a transaction. Zero is never a valid id, so a zeroed row
// version cannot be mistaken for one written by a real transaction.
type TxID uint64

// InvalidTxID is the zero value, used for "no transaction".
const InvalidTxID TxID = 0

// Status is the state of a transaction.
type Status uint8

const (
	// InProgress means the transaction has neither committed nor aborted.
	InProgress Status = iota
	// Committed means its writes are durable and visible to later snapshots.
	Committed
	// Aborted means its writes are invisible to everyone, forever.
	Aborted
)

func (s Status) String() string {
	switch s {
	case Committed:
		return "committed"
	case Aborted:
		return "aborted"
	default:
		return "in progress"
	}
}

// TxManager hands out transaction ids and remembers their outcome.
type TxManager struct {
	// mu guards all three fields together. They are one piece of state, not
	// three: a snapshot reads next as its XMax and active as its exclusion set,
	// so allocating an id outside this lock lets a snapshot exist whose XMax is
	// already past a transaction that is not yet in active. That transaction
	// then reads as finished-and-not-excluded, and becomes visible to the
	// snapshot the moment it commits — which silently breaks the stability that
	// REPEATABLE READ is.
	mu sync.RWMutex
	// next is the id the following transaction will receive.
	next   TxID
	status map[TxID]Status
	// active is the set of transactions that have not yet finished. It is kept
	// separately from status because taking a snapshot needs exactly this set
	// and nothing else.
	active map[TxID]bool
	// journal is where a committing transaction's changes are made durable, or
	// nil for an in-memory database. See SetJournal.
	journal Journal
}

// NewTxManager returns a manager whose first transaction will be id 1.
func NewTxManager() *TxManager {
	return &TxManager{
		next:   1,
		status: make(map[TxID]Status),
		active: make(map[TxID]bool),
	}
}

// Begin starts a transaction and returns its id.
func (m *TxManager) Begin() TxID {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := m.next
	m.next++
	m.status[id] = InProgress
	m.active[id] = true
	return id
}

// Commit marks a transaction committed, making its writes visible to snapshots
// taken afterwards.
func (m *TxManager) Commit(id TxID) { m.finish(id, Committed) }

// Abort marks a transaction aborted, making its writes invisible to everyone.
//
// This is the whole of rollback. Nothing is copied back and no changes are
// replayed in reverse, so a rollback cannot be partially applied.
func (m *TxManager) Abort(id TxID) { m.finish(id, Aborted) }

func (m *TxManager) finish(id TxID, s Status) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// An id that was never issued must stay unknown, which Status reports as
	// aborted. Writing a status for it here would invent a transaction and
	// could make rows referring to that id visible.
	current, ok := m.status[id]
	if !ok {
		return
	}
	if current != InProgress {
		// Committing or aborting twice is a no-op rather than an error, so that
		// a deferred Rollback after a Commit is harmless — which is the usual
		// shape of correct caller code.
		return
	}
	m.status[id] = s
	delete(m.active, id)
}

// Status reports a transaction's state. An id that was never issued reads as
// aborted, so a row referring to one is invisible rather than trusted.
func (m *TxManager) Status(id TxID) Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.status[id]
	if !ok {
		return Aborted
	}
	return s
}

// Horizon returns the id below which every transaction has finished.
//
// This is what Vacuum may safely reclaim up to. Using the next id instead would
// be too aggressive: a transaction still running holds a snapshot that can see
// versions younger than itself, and discarding those would make its rows vanish
// mid-scan.
func (m *TxManager) Horizon() TxID {
	m.mu.RLock()
	defer m.mu.RUnlock()

	oldest := m.next
	for id := range m.active {
		if id < oldest {
			oldest = id
		}
	}
	return oldest
}

// Snapshot is a consistent view of which transactions had finished at the moment
// it was taken.
type Snapshot struct {
	// XMin is the lowest id still active. Anything below it has finished, so it
	// can be judged by status alone.
	XMin TxID
	// XMax is the first id not yet issued. Anything at or above it started after
	// this snapshot and is therefore invisible.
	XMax TxID
	// active holds the transactions in progress when the snapshot was taken.
	// Their writes stay invisible even if they commit later, which is what makes
	// the view stable.
	active map[TxID]bool
	// mgr resolves the outcome of transactions that were not active.
	mgr *TxManager
}

// Take captures the current state as a snapshot.
func (m *TxManager) Take() *Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s := &Snapshot{
		XMax:   m.next,
		active: make(map[TxID]bool, len(m.active)),
		mgr:    m,
	}
	s.XMin = s.XMax
	for id := range m.active {
		s.active[id] = true
		if id < s.XMin {
			s.XMin = id
		}
	}
	return s
}

// committed reports whether a transaction's writes are visible to this snapshot.
//
// The three tests are ordered cheapest first, and each rules out a distinct
// case: started after the snapshot, still running when it was taken, or finished
// but rolled back.
func (s *Snapshot) committed(id TxID) bool {
	if id == InvalidTxID || id >= s.XMax {
		return false
	}
	if id >= s.XMin && s.active[id] {
		return false
	}
	return s.mgr.Status(id) == Committed
}

// ErrWriteConflict is returned when two transactions modify the same row and
// snapshot isolation cannot resolve the outcome. It carries PostgreSQL's
// serialization_failure code, which is the signal a caller retries on.
var ErrWriteConflict = pgerr.New(pgerr.SerializationFailure,
	"could not serialize access due to concurrent update")
