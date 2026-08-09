// Package engine ties the stages together: it owns a catalog and turns SQL text
// into results.
//
// It is deliberately free of database/sql types. The driver package adapts to
// that interface, which keeps the engine usable on its own and leaves room for a
// different front end later.
package engine

import (
	"context"

	"github.com/oxisto/lightsql/internal/binder"
	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/exec"
	"github.com/oxisto/lightsql/internal/parser"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/storage"
	"github.com/oxisto/lightsql/internal/types"
)

// Engine is one database instance.
type Engine struct {
	mgr *storage.TxManager
	cat *catalog.Catalog
	bnd *binder.Binder
}

// New returns an empty in-memory engine.
func New() *Engine {
	mgr := storage.NewTxManager()
	cat := catalog.New(mgr)
	return &Engine{mgr: mgr, cat: cat, bnd: binder.New(cat)}
}

// BeginTx starts an explicit transaction.
func (e *Engine) BeginTx(iso storage.Isolation, readOnly bool) *storage.Tx {
	return e.mgr.BeginTx(iso, readOnly)
}

// implicitTx wraps a single statement in its own transaction.
//
// Autocommit and an explicit transaction are then the same code path rather
// than two, which is what stops the two from drifting apart in their handling
// of visibility or constraint checking.
func (e *Engine) implicitTx() *storage.Tx {
	return e.mgr.BeginTx(storage.ReadCommitted, false)
}

// run executes fn inside tx when one is given, or inside a fresh implicit
// transaction otherwise, committing it on success and rolling it back on error.
func (e *Engine) withTx(tx *storage.Tx, fn func(*storage.Tx) error) error {
	if tx != nil {
		if err := tx.NextStatement(); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			// A failed statement poisons the transaction: PostgreSQL refuses
			// every later command until the caller rolls back, rather than
			// letting them build on a broken state.
			tx.Fail()
			return err
		}
		return nil
	}

	own := e.implicitTx()
	if err := fn(own); err != nil {
		_ = own.Rollback()
		return err
	}
	return own.Commit()
}

// Prepared is a statement that has been parsed and bound, ready to execute with
// arguments. Binding once and executing many times is only sound because the
// AST is never rewritten during execution.
type Prepared struct {
	eng *Engine
	// Params is the number of placeholders the statement expects.
	Params int
	stmt   plan.Stmt
}

// Prepare parses and binds a single statement.
func (e *Engine) Prepare(sql string) (*Prepared, error) {
	tree, err := parser.ParseOne(sql)
	if err != nil {
		return nil, err
	}
	bound, err := e.bnd.Bind(tree)
	if err != nil {
		return nil, err
	}
	return &Prepared{eng: e, Params: countParams(sql), stmt: bound}, nil
}

// ReturnsRows reports whether executing the statement produces a result set,
// which the driver needs in order to route Exec and Query correctly.
//
// A data-modifying statement returns rows exactly when it has a RETURNING
// clause, so this is not simply "is it a SELECT".
//
// This is deliberately distinct from IsQuery: `INSERT ... RETURNING` returns
// rows but is not a query, and the two need different handling when a caller
// asks for a row count.
func (p *Prepared) ReturnsRows() bool {
	switch s := p.stmt.(type) {
	case *plan.Query:
		return true
	case *plan.Insert:
		return s.Returning != nil
	case *plan.Update:
		return s.Returning != nil
	case *plan.Delete:
		return s.Returning != nil
	default:
		return false
	}
}

// IsQuery reports whether the statement is a SELECT. A data-modifying statement
// with a RETURNING clause also produces rows, but it still has a meaningful
// affected count, so the two cases are told apart here.
func (p *Prepared) IsQuery() bool {
	_, ok := p.stmt.(*plan.Query)
	return ok
}

// Exec runs a statement and reports how many rows it affected. Any RETURNING
// rows are discarded, matching what a caller using Exec has asked for.
func (p *Prepared) Exec(ctx context.Context, tx *storage.Tx, args []types.Value) (affected int64, err error) {
	res, _, err := p.run(ctx, tx, args)
	return res.Affected, err
}

// Query runs a statement and returns its rows.
func (p *Prepared) Query(ctx context.Context, tx *storage.Tx, args []types.Value) (*Rows, error) {
	if q, ok := p.stmt.(*plan.Query); ok {
		// A query is read-only, so its implicit transaction can be committed as
		// soon as the operator tree has taken its snapshot.
		var rows *Rows
		err := p.eng.withTx(tx, func(t *storage.Tx) error {
			op, err := exec.Build(q.Root, t, args)
			if err != nil {
				return err
			}
			rows = &Rows{cols: q.Root.Result(), op: op}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return rows, nil
	}

	res, ret, err := p.run(ctx, tx, args)
	if err != nil {
		return nil, err
	}
	if ret == nil {
		return nil, pgerr.New(pgerr.SyntaxError, "statement does not return rows; use Exec")
	}
	return &Rows{cols: ret.Cols, op: exec.NewSliceOp(res.Rows)}, nil
}

// run executes the statement, returning its result and the shape of any
// RETURNING clause. Both Exec and Query go through here so that the two cannot
// disagree about what a statement does.
func (p *Prepared) run(ctx context.Context, tx *storage.Tx, args []types.Value) (exec.Result, *plan.Returning, error) {
	var (
		res exec.Result
		ret *plan.Returning
	)
	err := p.eng.withTx(tx, func(t *storage.Tx) error {
		switch s := p.stmt.(type) {
		case *plan.CreateTable:
			return exec.ExecCreateTable(p.eng.cat, s)
		case *plan.Insert:
			if err := t.CheckWritable(); err != nil {
				return err
			}
			var err error
			res, ret = exec.Result{}, s.Returning
			res, err = exec.ExecInsert(ctx, t, s, args)
			return err
		case *plan.Update:
			if err := t.CheckWritable(); err != nil {
				return err
			}
			var err error
			ret = s.Returning
			res, err = exec.ExecUpdate(ctx, t, s, args)
			return err
		case *plan.Delete:
			if err := t.CheckWritable(); err != nil {
				return err
			}
			var err error
			ret = s.Returning
			res, err = exec.ExecDelete(ctx, t, s, args)
			return err
		default:
			return pgerr.New(pgerr.SyntaxError, "statement returns rows; use Query")
		}
	})
	return res, ret, err
}

// ExecBatch runs one or more statements separated by semicolons, returning the
// rows affected by the last one. It exists because test suites routinely set up
// a fixture with a single multi-statement Exec.
func (e *Engine) ExecBatch(ctx context.Context, tx *storage.Tx, sql string, args []types.Value) (int64, error) {
	trees, err := parser.Parse(sql)
	if err != nil {
		return 0, err
	}

	var affected int64
	for _, tree := range trees {
		bound, err := e.bnd.Bind(tree)
		if err != nil {
			return 0, err
		}
		p := &Prepared{eng: e, stmt: bound}
		// Only a genuine query is drained for its rows. A data-modifying
		// statement goes through Exec even when it has a RETURNING clause, so
		// that it still reports how many rows it changed.
		if p.IsQuery() {
			// A query inside a batch is legal but discards its rows, matching
			// what a server does for a multi-statement command.
			rows, err := p.Query(ctx, tx, args)
			if err != nil {
				return 0, err
			}
			err = rows.drain(ctx)
			_ = rows.Close()
			if err != nil {
				return 0, err
			}
			affected = 0
			continue
		}
		if affected, err = p.Exec(ctx, tx, args); err != nil {
			return 0, err
		}
	}
	return affected, nil
}

// Rows is a result set being read.
type Rows struct {
	cols []plan.ResultColumn
	op   exec.Operator
}

// Columns describes the result columns.
func (r *Rows) Columns() []plan.ResultColumn { return r.cols }

// Next returns the next row, which is only valid until the following call.
func (r *Rows) Next(ctx context.Context) (exec.Row, bool, error) { return r.op.Next(ctx) }

// Close releases the operator tree.
func (r *Rows) Close() error { return r.op.Close() }

func (r *Rows) drain(ctx context.Context) error {
	for {
		_, ok, err := r.op.Next(ctx)
		if err != nil || !ok {
			return err
		}
	}
}

// countParams reports how many distinct placeholders a statement uses, which
// database/sql needs from Stmt.NumInput to validate argument counts.
//
// It is derived from the scanned tokens rather than by counting occurrences of
// "$" in the text, which would also match inside string literals.
func countParams(sql string) int {
	toks, err := parser.ScanParams(sql)
	if err != nil {
		return -1 // unknown; database/sql then skips its own check
	}
	return toks
}
