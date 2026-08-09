// Package driver registers lightsql with database/sql under the name
// "lightsql".
//
// Importing it for its side effect is the convenient way in:
//
//	import _ "github.com/oxisto/lightsql/driver"
//
//	db, err := sql.Open("lightsql", "mytest")
//
// The package is a thin adapter. All it does is translate between database/sql's
// interfaces and the engine's, which is why it holds no query logic of its own.
package driver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"

	"github.com/oxisto/lightsql/internal/engine"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/storage"
	"github.com/oxisto/lightsql/internal/types"
)

func init() {
	sql.Register("lightsql", Driver{})
}

// Driver is the database/sql driver.
//
// It implements DriverContext so that database/sql parses a data source name
// once per sql.DB rather than once per connection.
type Driver struct{}

var (
	_ driver.Driver        = Driver{}
	_ driver.DriverContext = Driver{}
)

// Open implements driver.Driver.
func (d Driver) Open(dsn string) (driver.Conn, error) {
	c, err := d.OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

// OpenConnector implements driver.DriverContext.
func (d Driver) OpenConnector(dsn string) (driver.Connector, error) {
	return NewConnector(dsn)
}

// Connector resolves a data source name to an engine instance.
type Connector struct {
	driver Driver
	eng    *engine.Engine
}

var _ driver.Connector = (*Connector)(nil)

// NewConnector resolves a data source name.
func NewConnector(dsn string) (*Connector, error) {
	eng, err := instanceFor(dsn)
	if err != nil {
		return nil, err
	}
	return &Connector{eng: eng}, nil
}

// Connect implements driver.Connector.
func (c *Connector) Connect(context.Context) (driver.Conn, error) {
	return &Conn{eng: c.eng}, nil
}

// Driver implements driver.Connector.
func (c *Connector) Driver() driver.Driver { return c.driver }

// Conn is a connection to an engine instance.
//
// Connections to the same instance share its catalog and data, which is what
// makes database/sql's connection pool transparent: a test does not have to
// care which pooled connection ran which statement.
type Conn struct {
	eng *engine.Engine
	// tx is the explicit transaction this connection is inside, or nil when it
	// is in autocommit. database/sql guarantees a connection is used by one
	// goroutine at a time, so this needs no lock.
	tx     *storage.Tx
	closed bool
}

var (
	_ driver.Conn               = (*Conn)(nil)
	_ driver.ConnBeginTx        = (*Conn)(nil)
	_ driver.ConnPrepareContext = (*Conn)(nil)
	_ driver.ExecerContext      = (*Conn)(nil)
	_ driver.QueryerContext     = (*Conn)(nil)
	_ driver.Pinger             = (*Conn)(nil)
	_ driver.SessionResetter    = (*Conn)(nil)
	_ driver.Validator          = (*Conn)(nil)
	_ driver.NamedValueChecker  = (*Conn)(nil)
)

// Prepare implements driver.Conn.
func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext implements driver.ConnPrepareContext.
func (c *Conn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	p, err := c.eng.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &Stmt{prepared: p, conn: c}, nil
}

// Close implements driver.Conn. The engine outlives the connection, so this only
// marks the connection unusable.
func (c *Conn) Close() error {
	// Closing with a transaction open rolls it back, rather than leaving its
	// writes in limbo where a later snapshot might still be deciding about them.
	if c.tx != nil {
		_ = c.tx.Rollback()
		c.tx = nil
	}
	c.closed = true
	return nil
}

// Begin implements driver.Conn.
func (c *Conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx implements driver.ConnBeginTx.
//
// The requested isolation level is honoured rather than accepted and ignored: a
// caller that asks for REPEATABLE READ and silently gets READ COMMITTED has no
// way to discover the difference except by hitting a bug in production.
func (c *Conn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.closed {
		return nil, driver.ErrBadConn
	}
	if c.tx != nil {
		return nil, pgerr.New(pgerr.InvalidTransactionState,
			"there is already a transaction in progress")
	}
	iso, err := isolationOf(opts.Isolation)
	if err != nil {
		return nil, err
	}
	c.tx = c.eng.BeginTx(iso, opts.ReadOnly)
	return &Tx{conn: c}, nil
}

// isolationOf maps database/sql's levels onto the engine's.
//
// Levels weaker than READ COMMITTED are raised to it rather than rejected:
// lightsql never shows uncommitted data, so READ UNCOMMITTED is satisfied by
// giving something stronger, which the standard explicitly permits.
func isolationOf(level driver.IsolationLevel) (storage.Isolation, error) {
	switch sql.IsolationLevel(level) {
	case sql.LevelDefault, sql.LevelReadUncommitted, sql.LevelReadCommitted:
		return storage.ReadCommitted, nil
	case sql.LevelRepeatableRead, sql.LevelSnapshot:
		return storage.RepeatableRead, nil
	case sql.LevelSerializable, sql.LevelLinearizable:
		return storage.Serializable, nil
	default:
		return 0, pgerr.Newf(pgerr.FeatureNotSupported,
			"isolation level %s is not supported", sql.IsolationLevel(level))
	}
}

// Tx is an explicit transaction. It exists as its own type, rather than the
// connection standing in for one, so that a transaction cannot outlive the
// statement handling that owns it.
type Tx struct{ conn *Conn }

var _ driver.Tx = (*Tx)(nil)

// Commit implements driver.Tx.
func (t *Tx) Commit() error {
	tx := t.conn.tx
	if tx == nil {
		return pgerr.New(pgerr.NoActiveTransaction, "there is no transaction in progress")
	}
	t.conn.tx = nil
	return tx.Commit()
}

// Rollback implements driver.Tx.
func (t *Tx) Rollback() error {
	tx := t.conn.tx
	if tx == nil {
		return pgerr.New(pgerr.NoActiveTransaction, "there is no transaction in progress")
	}
	t.conn.tx = nil
	return tx.Rollback()
}

// Ping implements driver.Pinger.
func (c *Conn) Ping(context.Context) error {
	if c.closed {
		return driver.ErrBadConn
	}
	return nil
}

// IsValid implements driver.Validator, so a closed connection is discarded from
// the pool rather than handed out again.
func (c *Conn) IsValid() bool { return !c.closed }

// ResetSession implements driver.SessionResetter.
//
// A transaction left open is rolled back before the connection is handed to the
// next user. Without this a leaked transaction would keep its snapshot and its
// locks alive, and the next caller would silently inherit them.
func (c *Conn) ResetSession(context.Context) error {
	if c.closed {
		return driver.ErrBadConn
	}
	if c.tx != nil {
		_ = c.tx.Rollback()
		c.tx = nil
	}
	return nil
}

// CheckNamedValue implements driver.NamedValueChecker.
//
// Implementing it takes over argument conversion from database/sql's default,
// which lets a driver.Valuer and the ordinary Go scalar types both work while
// rejecting anything else here, with a clear error, rather than deep in
// execution.
func (c *Conn) CheckNamedValue(nv *driver.NamedValue) error {
	if nv.Name != "" {
		return pgerr.New(pgerr.FeatureNotSupported, "named parameters are not supported yet")
	}
	v, err := driver.DefaultParameterConverter.ConvertValue(nv.Value)
	if err != nil {
		return err
	}
	nv.Value = v
	return nil
}

// ExecContext implements driver.ExecerContext. It accepts a batch of
// semicolon-separated statements, which is what makes a fixture setup a single
// call.
func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	vals, err := convertArgs(args)
	if err != nil {
		return nil, err
	}
	affected, err := c.eng.ExecBatch(ctx, c.tx, query, vals)
	if err != nil {
		return nil, err
	}
	return result(affected), nil
}

// QueryContext implements driver.QueryerContext.
func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	p, err := c.eng.Prepare(query)
	if err != nil {
		return nil, err
	}
	return queryPrepared(ctx, p, c.tx, args)
}

// Stmt is a prepared statement. It is bound once and may be executed repeatedly,
// which is only sound because nothing rewrites the plan during execution.
type Stmt struct {
	prepared *engine.Prepared
	// conn is retained so that executing the statement joins whatever
	// transaction the connection is currently in.
	conn *Conn
}

var (
	_ driver.Stmt             = (*Stmt)(nil)
	_ driver.StmtExecContext  = (*Stmt)(nil)
	_ driver.StmtQueryContext = (*Stmt)(nil)
)

// Close implements driver.Stmt.
func (s *Stmt) Close() error { return nil }

// NumInput implements driver.Stmt, letting database/sql check the argument count
// before it reaches the engine.
func (s *Stmt) NumInput() int { return s.prepared.Params }

// Exec implements driver.Stmt.
func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), positional(args))
}

// Query implements driver.Stmt.
func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), positional(args))
}

// ExecContext implements driver.StmtExecContext.
func (s *Stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	vals, err := convertArgs(args)
	if err != nil {
		return nil, err
	}
	affected, err := s.prepared.Exec(ctx, s.conn.tx, vals)
	if err != nil {
		return nil, err
	}
	return result(affected), nil
}

// QueryContext implements driver.StmtQueryContext.
func (s *Stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return queryPrepared(ctx, s.prepared, s.conn.tx, args)
}

func queryPrepared(ctx context.Context, p *engine.Prepared, tx *storage.Tx, args []driver.NamedValue) (driver.Rows, error) {
	vals, err := convertArgs(args)
	if err != nil {
		return nil, err
	}
	rows, err := p.Query(ctx, tx, vals)
	if err != nil {
		return nil, err
	}
	return &Rows{ctx: ctx, rows: rows, cols: rows.Columns()}, nil
}

// Rows is a result set.
type Rows struct {
	// ctx is retained so that cancellation reaches the operator tree on every
	// Next, not only on the call that started the query.
	ctx  context.Context
	rows *engine.Rows
	cols []plan.ResultColumn
}

var (
	_ driver.Rows                           = (*Rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*Rows)(nil)
	_ driver.RowsColumnTypeScanType         = (*Rows)(nil)
	_ driver.RowsColumnTypeNullable         = (*Rows)(nil)
)

// Columns implements driver.Rows.
func (r *Rows) Columns() []string {
	names := make([]string, len(r.cols))
	for i, c := range r.cols {
		names[i] = c.Name
	}
	return names
}

// Close implements driver.Rows.
func (r *Rows) Close() error { return r.rows.Close() }

// Next implements driver.Rows.
func (r *Rows) Next(dest []driver.Value) error {
	row, ok, err := r.rows.Next(r.ctx)
	if err != nil {
		return err
	}
	if !ok {
		return io.EOF
	}
	// The engine reuses its output row between calls, so the values are copied
	// out here rather than aliased.
	for i := range dest {
		dest[i] = engine.ToDriver(row[i])
	}
	return nil
}

// ColumnTypeDatabaseTypeName implements driver.RowsColumnTypeDatabaseTypeName.
// ORMs read this when deciding how to map a column.
func (r *Rows) ColumnTypeDatabaseTypeName(i int) string { return r.cols[i].Type.Name }

// ColumnTypeScanType implements driver.RowsColumnTypeScanType.
func (r *Rows) ColumnTypeScanType(i int) reflect.Type {
	return engine.ScanType(r.cols[i].Type, true)
}

// ColumnTypeNullable implements driver.RowsColumnTypeNullable. Nullability is
// not yet tracked through a projection, so this reports unknown rather than
// claiming a column cannot be NULL.
func (r *Rows) ColumnTypeNullable(int) (nullable, ok bool) { return true, false }

// result reports rows affected. LastInsertId is deliberately unsupported:
// PostgreSQL has no such concept, and RETURNING is the portable way to get a
// generated key.
type result int64

var _ driver.Result = result(0)

func (r result) LastInsertId() (int64, error) {
	return 0, errors.New("lightsql: LastInsertId is not supported; use RETURNING")
}

func (r result) RowsAffected() (int64, error) { return int64(r), nil }

func convertArgs(args []driver.NamedValue) ([]types.Value, error) {
	if len(args) == 0 {
		return nil, nil
	}
	// Ordinals are 1-based and need not arrive in order, so place each argument
	// by its ordinal rather than by its position in the slice.
	highest := 0
	for _, a := range args {
		if a.Ordinal > highest {
			highest = a.Ordinal
		}
	}
	vals := make([]types.Value, highest)
	for i := range vals {
		vals[i] = types.Null()
	}
	for _, a := range args {
		v, err := engine.FromDriver(a.Value)
		if err != nil {
			return nil, err
		}
		vals[a.Ordinal-1] = v
	}
	return vals, nil
}

func positional(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return out
}
