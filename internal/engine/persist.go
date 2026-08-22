package engine

import (
	"context"
	"maps"
	"slices"

	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/exec"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/storage"
	"github.com/oxisto/lightsql/internal/types"
	"github.com/oxisto/lightsql/internal/wal"
)

// Open returns an engine backed by the database directory dir, rebuilt from
// whatever is already there.
//
// Recovery runs before the log is attached, and that order is the whole trick:
// replaying a statement uses exactly the same catalog and storage code as
// executing one, so a log attached first would write everything it had just
// read straight back and double the file on every restart.
//
// When fsync is set a commit does not return until its records have reached the
// disk. That is the only setting under which a crash cannot lose an
// acknowledged transaction, which is why it is what a file-backed database
// gets by default.
func Open(dir string, fsync bool) (*Engine, error) {
	log, err := wal.Open(dir, fsync)
	if err != nil {
		return nil, err
	}

	e := New()
	if err := e.recover(log); err != nil {
		_ = log.Close()
		return nil, err
	}
	e.log = log
	e.mgr.SetJournal(log)
	return e, nil
}

// Checkpoint rewrites the log as the database's current state rather than the
// history that produced it.
//
// Without it the log grows for as long as the database is written to, and a
// restart replays every change ever made instead of the rows that survived
// them. The schema half is the statements that built it, because the catalog
// keeps DEFAULT expressions and CHECK predicates as syntax and there is nothing
// else to write them back out from.
func (e *Engine) Checkpoint() error {
	if e.log == nil {
		return nil
	}

	// One snapshot for the whole checkpoint, so the file describes a state the
	// database was really in rather than a blend of several.
	tx := e.mgr.BeginTx(storage.RepeatableRead, true)
	defer func() { _ = tx.Rollback() }()

	e.schemaMu.Lock()
	recs := slices.Clone(e.schema)
	e.schemaMu.Unlock()

	for _, t := range e.cat.Tables() {
		name := t.QualifiedName()
		for _, v := range t.Scan(tx) {
			// Values pads a row written before an ADD COLUMN out to the table's
			// width. Writing the padded row is deliberate: it is what every
			// reader already sees, and it means the padding is done once here
			// rather than on every read for the rest of the database's life.
			recs = append(recs, wal.InsertRecord(name, uint64(v.ID), t.Values(v)))
		}
	}
	return e.log.Checkpoint(recs)
}

// Close checkpoints and releases the database directory.
//
// The journal is detached first so that a transaction still in flight fails to
// commit rather than writing to a closed file.
func (e *Engine) Close() error {
	if e.log == nil {
		return nil
	}
	err := e.Checkpoint()
	e.mgr.SetJournal(nil)
	log := e.log
	e.log = nil
	if cerr := log.Close(); err == nil {
		err = cerr
	}
	return err
}

// recover rebuilds the engine from the log.
//
// Rows are collected as they are read and placed only at the end. A delete is
// then dropping a map entry rather than searching a heap for the version that
// carries an id, which is what keeps recovery linear in the size of the log
// rather than quadratic. It also means a row that was inserted and deleted
// again never reaches the heap at all.
func (e *Engine) recover(log *wal.Log) error {
	ctx := context.Background()
	boot := e.mgr.BeginTx(storage.ReadCommitted, false)

	e.recovering = true
	defer func() { e.recovering = false }()

	rows := map[*catalog.Table]map[uint64][]types.Value{}
	err := log.Replay(func(recs []wal.Record) error {
		for i := range recs {
			if err := e.apply(ctx, boot, &recs[i], rows); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = boot.Rollback()
		return err
	}

	for _, t := range e.cat.Tables() {
		held := rows[t]
		// Ascending id is the order the rows were written in, so a table reads
		// back the way it did before the restart. Nothing promises that order,
		// but silently changing it across a restart would be a surprise nobody
		// asked for.
		for _, id := range slices.Sorted(maps.Keys(held)) {
			t.Load(boot, storage.RowID(id), held[id])
		}
		// A sequence is not logged; it is derived from the rows. See
		// Table.RestoreSequences for what that costs.
		t.RestoreSequences(boot)
	}
	return boot.Commit()
}

// apply replays one record.
func (e *Engine) apply(ctx context.Context, tx *storage.Tx, rec *wal.Record, rows map[*catalog.Table]map[uint64][]types.Value) error {
	switch rec.Kind {
	case wal.DDL:
		if _, err := e.ExecBatch(ctx, tx, rec.SQL, nil); err != nil {
			return pgerr.Newf(pgerr.DataCorrupted,
				"replaying %q from the write-ahead log: %v", rec.SQL, err)
		}
		e.rememberSchema(*rec)
		return nil

	case wal.Missing:
		t, err := e.cat.LookupQualified(rec.Table)
		if err != nil {
			return err
		}
		if err := t.SetMissing(int(rec.Column), rec.Vals[0]); err != nil {
			return err
		}
		e.rememberSchema(*rec)
		return nil

	case wal.Insert:
		t, err := e.cat.LookupQualified(rec.Table)
		if err != nil {
			return err
		}
		held, ok := rows[t]
		if !ok {
			held = make(map[uint64][]types.Value)
			rows[t] = held
		}
		held[rec.Row] = rec.Vals
		return nil

	default:
		t, err := e.cat.LookupQualified(rec.Table)
		if err != nil {
			return err
		}
		delete(rows[t], rec.Row)
		return nil
	}
}

// isDDL reports whether a statement changes the schema.
//
// The set is written out rather than inferred, because everything in it is
// logged as text and everything outside it as values. A new schema statement
// that is not added here executes correctly and then disappears at the next
// restart, which is the kind of omission that shows up much later.
func isDDL(s plan.Stmt) bool {
	switch s.(type) {
	case *plan.CreateTable, *plan.AddColumn, *plan.RenameTable, *plan.RenameColumn,
		*plan.CreateIndex, *plan.DropIndex, *plan.DropTable:
		return true
	default:
		return false
	}
}

// execDDL runs a schema statement and logs it, reporting whether the statement
// was one.
func (p *Prepared) execDDL(t *storage.Tx) (handled bool, err error) {
	if !isDDL(p.stmt) {
		return false, nil
	}
	// DDL is a write too. Leaving this out let a read-only transaction reshape
	// the catalog, which contradicts what ReadOnly promises.
	if err := t.CheckWritable(); err != nil {
		return true, err
	}

	cat := p.eng.cat
	// extra holds what replaying the statement would not reproduce.
	var extra []wal.Record

	switch s := p.stmt.(type) {
	case *plan.CreateTable:
		err = exec.ExecCreateTable(cat, s)

	case *plan.AddColumn:
		if err = exec.ExecAddColumn(cat, s); err == nil {
			// The value on the column is logged rather than the one the
			// statement carries. With IF NOT EXISTS the statement may have done
			// nothing at all, and then the value already there is the true one.
			if ord := s.Table.ColumnIndex(s.Column.Name); ord >= 0 {
				extra = append(extra, wal.MissingRecord(
					s.Table.QualifiedName(), ord, s.Table.Columns[ord].Missing))
			}
		}

	case *plan.RenameTable:
		if err = exec.ExecRenameTable(cat, s); err == nil {
			// Rows this transaction has already written are buffered under the
			// old name, and the rename is in the log ahead of them. By the time
			// recovery reaches those rows the old name is gone, so they are
			// corrected here.
			t.RenameLogged(catalog.Qualify(s.Schema, s.From), catalog.Qualify(s.Schema, s.To))
		}

	case *plan.RenameColumn:
		err = exec.ExecRenameColumn(cat, s)

	case *plan.CreateIndex:
		err = exec.ExecCreateIndex(cat, s)

	case *plan.DropIndex:
		err = exec.ExecDropIndex(cat, s)

	case *plan.DropTable:
		if err = exec.ExecDropTable(cat, s); err == nil {
			gone := make([]string, len(s.Names))
			for i, n := range s.Names {
				gone[i] = catalog.Qualify(n.Schema, n.Name)
			}
			// The rows went with the table in memory; replaying them into a
			// table that no longer exists would fail recovery over work that
			// was already discarded.
			t.ForgetLogged(gone...)
		}
	}
	if err != nil {
		return true, err
	}
	return true, p.eng.recordSchema(t, append([]wal.Record{wal.DDLRecord(p.sql)}, extra...))
}

// recordSchema remembers a schema change and writes it to the log at once,
// rather than at commit like a row.
//
// DDL is not transactional in lightsql: a CREATE TABLE in a transaction that
// later rolls back is still there afterwards. Buffering it would make the log
// disagree with the running database about exactly those statements.
func (e *Engine) recordSchema(t *storage.Tx, recs []wal.Record) error {
	if e.recovering {
		// The log is being read, not written. Its own records are the history,
		// and they are appended as they are applied -- re-recording what
		// replaying them produced would double the schema and, for ADD COLUMN,
		// record a missing value computed at recovery time.
		return nil
	}

	e.rememberSchema(recs...)
	return t.LogNow(recs...)
}

// rememberSchema adds to the history a checkpoint will write back out.
func (e *Engine) rememberSchema(recs ...wal.Record) {
	e.schemaMu.Lock()
	defer e.schemaMu.Unlock()
	e.schema = append(e.schema, recs...)
}
