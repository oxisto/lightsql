package storage

import (
	"time"

	"github.com/oxisto/lightsql/internal/pgerr"
)

// Isolation is the isolation level a transaction runs at.
//
// It is defined here rather than reusing database/sql's sql.IsolationLevel
// because the engine stays free of database/sql types: the driver maps between
// the two, and nothing below the driver knows that package exists.
type Isolation uint8

const (
	// ReadCommitted takes a fresh snapshot for every statement, so each one sees
	// everything committed before it started. This is PostgreSQL's default and
	// lightsql's.
	ReadCommitted Isolation = iota
	// RepeatableRead takes one snapshot for the whole transaction, so every
	// statement in it sees the same data.
	RepeatableRead
	// Serializable behaves as RepeatableRead here. True serializability needs
	// predicate tracking to detect write skew, which is not implemented; the
	// level is accepted rather than rejected because rejecting it would break
	// callers that ask for the strictest level out of caution.
	Serializable
)

func (i Isolation) String() string {
	switch i {
	case RepeatableRead:
		return "repeatable read"
	case Serializable:
		return "serializable"
	default:
		return "read committed"
	}
}

// Tx is the transaction a statement runs within.
//
// Every statement runs inside one. A statement issued outside an explicit
// BEGIN gets an implicit transaction of its own, which is what makes autocommit
// and an explicit transaction the same code path rather than two.
type Tx struct {
	ID  TxID
	mgr *TxManager

	iso      Isolation
	readOnly bool
	// snap is the view this transaction reads through. Under ReadCommitted it is
	// replaced at each statement; otherwise it is taken once and kept.
	snap *Snapshot
	// failed records that a statement errored. PostgreSQL puts a transaction
	// into a failed state where every later statement is refused until the
	// caller rolls back, rather than letting them build on a broken one.
	failed bool
	done   bool
	// started is when the transaction began. now() reports it rather than the
	// wall clock, so every statement in a transaction agrees about "now" --
	// which is what PostgreSQL guarantees and what stops two rows inserted by
	// one transaction from disagreeing about when they were written.
	started time.Time
}

// Begin starts a transaction at the given isolation level.
func (m *TxManager) BeginTx(iso Isolation, readOnly bool) *Tx {
	tx := &Tx{ID: m.Begin(), mgr: m, iso: iso, readOnly: readOnly, started: time.Now().UTC()}
	tx.snap = m.Take()
	return tx
}

// Started reports when the transaction began.
func (t *Tx) Started() time.Time { return t.started }

// Snapshot returns the view this transaction reads through.
func (t *Tx) Snapshot() *Snapshot { return t.snap }

// Isolation reports the level the transaction runs at.
func (t *Tx) Isolation() Isolation { return t.iso }

// ReadOnly reports whether the transaction refuses writes.
func (t *Tx) ReadOnly() bool { return t.readOnly }

// NextStatement prepares the transaction for another statement.
//
// Under ReadCommitted this refreshes the snapshot, which is the whole of what
// distinguishes it from RepeatableRead: the same query run twice in one
// transaction may legitimately give different answers, because it sees whatever
// committed in between.
func (t *Tx) NextStatement() error {
	if t.done {
		return pgerr.New(pgerr.InvalidTransactionState, "transaction has already finished")
	}
	if t.failed {
		return pgerr.New(pgerr.InFailedTransaction,
			"current transaction is aborted, commands ignored until end of transaction block")
	}
	if t.iso == ReadCommitted {
		t.snap = t.mgr.Take()
	}
	return nil
}

// Fail marks the transaction as broken by a failed statement.
func (t *Tx) Fail() { t.failed = true }

// Failed reports whether a statement has errored in this transaction.
func (t *Tx) Failed() bool { return t.failed }

// CheckWritable rejects a write in a read-only transaction.
func (t *Tx) CheckWritable() error {
	if t.readOnly {
		return pgerr.New(pgerr.ReadOnlySQLTransaction,
			"cannot execute a data-modifying statement in a read-only transaction")
	}
	return nil
}

// Commit makes the transaction's writes visible to later snapshots.
//
// A transaction that has already failed cannot commit: PostgreSQL turns such a
// commit into a rollback rather than silently keeping part of the work.
func (t *Tx) Commit() error {
	if t.done {
		return pgerr.New(pgerr.InvalidTransactionState, "transaction has already finished")
	}
	t.done = true
	if t.failed {
		t.mgr.Abort(t.ID)
		return pgerr.New(pgerr.InFailedTransaction,
			"current transaction is aborted, commands ignored until end of transaction block")
	}
	t.mgr.Commit(t.ID)
	return nil
}

// Rollback discards the transaction's writes.
//
// It is safe to call after Commit, so that a deferred rollback in caller code
// is harmless.
func (t *Tx) Rollback() error {
	if t.done {
		return nil
	}
	t.done = true
	t.mgr.Abort(t.ID)
	return nil
}

// Done reports whether the transaction has committed or rolled back.
func (t *Tx) Done() bool { return t.done }
