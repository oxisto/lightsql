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
	"github.com/oxisto/lightsql/internal/types"
)

// Engine is one database instance.
type Engine struct {
	cat *catalog.Catalog
	bnd *binder.Binder
}

// New returns an empty in-memory engine.
func New() *Engine {
	cat := catalog.New()
	return &Engine{cat: cat, bnd: binder.New(cat)}
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
func (p *Prepared) ReturnsRows() bool {
	_, ok := p.stmt.(*plan.Query)
	return ok
}

// Exec runs a statement that does not return rows.
func (p *Prepared) Exec(ctx context.Context, args []types.Value) (affected int64, err error) {
	switch s := p.stmt.(type) {
	case *plan.CreateTable:
		return 0, exec.ExecCreateTable(p.eng.cat, s)
	case *plan.Insert:
		return exec.ExecInsert(ctx, s, args)
	default:
		return 0, pgerr.New(pgerr.SyntaxError, "statement returns rows; use Query")
	}
}

// Query runs a statement that returns rows.
func (p *Prepared) Query(ctx context.Context, args []types.Value) (*Rows, error) {
	q, ok := p.stmt.(*plan.Query)
	if !ok {
		return nil, pgerr.New(pgerr.SyntaxError, "statement does not return rows; use Exec")
	}
	op, err := exec.Build(q.Root, args)
	if err != nil {
		return nil, err
	}
	return &Rows{cols: q.Root.Result(), op: op}, nil
}

// ExecBatch runs one or more statements separated by semicolons, returning the
// rows affected by the last one. It exists because test suites routinely set up
// a fixture with a single multi-statement Exec.
func (e *Engine) ExecBatch(ctx context.Context, sql string, args []types.Value) (int64, error) {
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
		if p.ReturnsRows() {
			// A query inside a batch is legal but discards its rows, matching
			// what a server does for a multi-statement command.
			rows, err := p.Query(ctx, args)
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
		if affected, err = p.Exec(ctx, args); err != nil {
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
