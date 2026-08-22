package storage

import (
	"slices"

	"github.com/oxisto/lightsql/internal/wal"
)

// Journal is where a committing transaction's changes are made durable.
//
// It is an interface so that an in-memory database has one fewer thing to do
// rather than one more thing to configure: a manager with no journal simply
// does not record anything, and every path below here is identical either way.
type Journal interface {
	// Write makes one transaction's records durable. It must return only once
	// they are, because the caller marks the transaction committed on the
	// strength of it.
	Write(recs []wal.Record) error
}

// SetJournal installs the journal committed transactions are written to.
//
// It is set after recovery rather than at construction, and that ordering
// matters: replaying the log runs the same catalog and storage code as an
// ordinary statement, so a journal attached beforehand would write everything it
// had just read straight back, doubling the log on every restart.
func (m *TxManager) SetJournal(j Journal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journal = j
}

// journalOf returns the installed journal, or nil. A transaction reads it once,
// when it begins, rather than per row: the journal is installed at startup and
// not changed afterwards, so taking the manager's lock for every value written
// would buy nothing.
func (m *TxManager) journalOf() Journal {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.journal
}

// Log records a change to be made durable when the transaction commits.
//
// Buffering until commit is what keeps a rolled back transaction out of the log
// entirely. In memory a rollback costs nothing because nothing was overwritten;
// on disk it costs nothing because nothing was written.
func (t *Tx) Log(rec wal.Record) {
	if t.journal == nil {
		return
	}
	t.pending = append(t.pending, rec)
}

// LogNow makes a change durable immediately, without waiting for the commit.
//
// This is for DDL, which lightsql applies to the catalog as it runs rather than
// at commit: a CREATE TABLE in a transaction that later rolls back is still
// there afterwards. Buffering it would mean the log disagreed with the running
// database about exactly the statements where the disagreement is permanent.
// It takes several records because a statement can need more than one -- ADD
// COLUMN logs both the statement and the value it computed for the rows that
// predate it -- and they must reach the log together. Written separately, a
// crash between them would replay the statement without the value it produced.
func (t *Tx) LogNow(recs ...wal.Record) error {
	if t.journal == nil || len(recs) == 0 {
		return nil
	}
	return t.journal.Write(recs)
}

// RenameLogged rewrites the buffered records that name a table which has just
// been renamed.
//
// A record names its table by name, and DDL is written to the log as it runs
// while rows wait for the commit. So `BEGIN; INSERT INTO t ...; ALTER TABLE t
// RENAME TO u; COMMIT` puts the rename in the log first, and by the time
// recovery reaches the buffered rows there is no table called t any more. The
// buffer is small and a rename is rare, so correcting it here costs nothing and
// keeps the alternative -- naming tables by a number whose meaning depends on
// replaying the log in exactly the right order -- off the table. A wrong name
// fails recovery loudly; a wrong number would quietly fill the wrong table.
func (t *Tx) RenameLogged(from, to string) {
	for i := range t.pending {
		if t.pending[i].Table == from {
			t.pending[i].Table = to
		}
	}
}

// ForgetLogged discards the buffered records naming a table that has just been
// dropped, for the same reason RenameLogged rewrites them. The rows went with
// the table in memory, and replaying them into a table that no longer exists
// would fail recovery over work that was already discarded.
func (t *Tx) ForgetLogged(names ...string) {
	t.pending = slices.DeleteFunc(t.pending, func(r wal.Record) bool {
		return slices.Contains(names, r.Table)
	})
}

// flush writes the buffered records, and is called by Commit before the
// transaction is marked committed. A transaction whose changes cannot be made
// durable must not be reported as committed; acknowledging first and writing
// afterwards is how a database loses work it said it had.
func (t *Tx) flush() error {
	if len(t.pending) == 0 || t.journal == nil {
		return nil
	}
	err := t.journal.Write(t.pending)
	t.pending = nil
	return err
}
