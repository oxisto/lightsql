// Package catalog holds the schema metadata: which tables exist, what columns
// they have, and what constraints apply.
//
// The catalog is plain in-memory structs. It is deliberately not stored as
// ordinary tables that DDL inserts rows into, because that makes every CREATE
// TABLE take data locks on a shared relation and turns a catalog update into a
// query that can fail halfway. information_schema and pg_catalog are instead
// exposed as read-only views computed from these structs.
package catalog

import (
	"strings"
	"sync"

	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/types"
)

// DefaultSchema is the schema a name resolves to when written unqualified.
const DefaultSchema = "public"

// Column is one column of a table.
type Column struct {
	Name string
	Type Type
	// NotNull is set by a NOT NULL constraint or implied by PRIMARY KEY.
	NotNull bool
	// PrimaryKey marks membership of the table's primary key.
	PrimaryKey bool
	// Unique marks a single-column UNIQUE constraint.
	Unique bool
}

// Table is a table definition together with its data.
//
// Definition and storage live in one struct because a table's rows are only ever
// reached through its definition, and separating them would mean every access
// carried a pair of pointers that must not disagree.
type Table struct {
	Schema string
	Name   string

	Columns []Column
	// byName resolves a column name to its ordinal. Every reference below the
	// binder is an ordinal; this map is consulted during binding only.
	byName map[string]int

	// mu guards rows. Storage is a slot slice rather than a linked list: rows
	// are read far more often than they are deleted, and a slice keeps them
	// contiguous instead of costing four words of pointers each.
	mu   sync.RWMutex
	rows [][]types.Value
	// nextSerial holds the next value for each serial column, keyed by ordinal.
	// A sequence is per column rather than global so that truncating one table
	// cannot disturb another.
	nextSerial map[int]int64
}

// QualifiedName returns the schema-qualified name, as it appears in errors.
func (t *Table) QualifiedName() string { return t.Schema + "." + t.Name }

// ColumnIndex returns the ordinal of a column, or -1 if there is none.
func (t *Table) ColumnIndex(name string) int {
	if i, ok := t.byName[name]; ok {
		return i
	}
	return -1
}

// Insert appends a row. The row must already be the right width and have been
// coerced to each column's type by the binder.
func (t *Table) Insert(row []types.Value) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := t.checkRow(row); err != nil {
		return err
	}
	t.rows = append(t.rows, row)
	return nil
}

// Rows returns a snapshot of the table's rows.
//
// The slice header is copied under the lock, so a scan iterates a stable view
// while other statements append. This is a placeholder for real MVCC snapshots:
// it gives a reader a consistent length, but not yet isolation from updates to
// rows it has already seen.
func (t *Table) Rows() [][]types.Value {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.rows[:len(t.rows):len(t.rows)]
}

// Mutate applies a function to every row under the table's write lock.
//
// The callback returns the row's replacement, or nil to delete it. Modifying
// rows this way, rather than handing out the slice, keeps two properties that
// matter:
//
//   - The whole statement observes one consistent state, because the lock is
//     held for its duration.
//   - Readers that already obtained a snapshot from Rows are unaffected, since
//     both the row slice and each replaced row are copied rather than written
//     through. That is a weak stand-in for the MVCC snapshots to come, but it
//     stops a concurrent scan from seeing a half-applied UPDATE.
//
// The callback must not call back into the table.
func (t *Table) Mutate(fn func(i int, row []types.Value) (replacement []types.Value, err error)) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	next := make([][]types.Value, 0, len(t.rows))
	for i, row := range t.rows {
		replacement, err := fn(i, row)
		if err != nil {
			return err
		}
		if replacement == nil {
			continue // deleted
		}
		if err := t.checkRow(replacement); err != nil {
			return err
		}
		next = append(next, replacement)
	}
	t.rows = next
	return nil
}

// checkRow enforces the constraints that apply to a stored row. The caller must
// hold the write lock.
func (t *Table) checkRow(row []types.Value) error {
	for i, col := range t.Columns {
		if col.NotNull && row[i].IsNull() {
			return pgerr.Newf(pgerr.NotNullViolation,
				"null value in column %q of relation %q violates not-null constraint",
				col.Name, t.Name)
		}
	}
	return nil
}

// NextSerial returns and consumes the next value of a serial column.
func (t *Table) NextSerial(ordinal int) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.nextSerial == nil {
		t.nextSerial = make(map[int]int64)
	}
	n := t.nextSerial[ordinal]
	if n == 0 {
		n = 1 // sequences start at 1, matching PostgreSQL
	}
	t.nextSerial[ordinal] = n + 1
	return n
}

// Catalog is the set of tables in one database instance.
type Catalog struct {
	mu     sync.RWMutex
	tables map[string]*Table
}

// New returns an empty catalog.
func New() *Catalog {
	return &Catalog{tables: make(map[string]*Table)}
}

func key(schema, name string) string { return schema + "." + name }

// CreateTable registers a new table. It reports a duplicate as a
// DuplicateTable error unless ifNotExists is set, in which case an existing
// table is left alone and created reports false.
func (c *Catalog) CreateTable(t *Table, ifNotExists bool) (created bool, err error) {
	if t.Schema == "" {
		t.Schema = DefaultSchema
	}
	if err := t.index(); err != nil {
		return false, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	k := key(t.Schema, t.Name)
	if _, exists := c.tables[k]; exists {
		if ifNotExists {
			return false, nil
		}
		return false, pgerr.Newf(pgerr.DuplicateTable, "relation %q already exists", t.Name)
	}
	c.tables[k] = t
	return true, nil
}

// index builds the name lookup and rejects duplicate column names.
func (t *Table) index() error {
	t.byName = make(map[string]int, len(t.Columns))
	for i, col := range t.Columns {
		if _, dup := t.byName[col.Name]; dup {
			return pgerr.Newf(pgerr.DuplicateColumn,
				"column %q specified more than once", col.Name)
		}
		t.byName[col.Name] = i
	}
	return nil
}

// Lookup finds a table. An empty schema means the default schema.
func (c *Catalog) Lookup(schema, name string) (*Table, error) {
	if schema == "" {
		schema = DefaultSchema
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	t, ok := c.tables[key(schema, name)]
	if !ok {
		return nil, pgerr.Newf(pgerr.UndefinedTable, "relation %q does not exist", name)
	}
	return t, nil
}

// Tables returns every table, ordered by qualified name, for introspection.
func (c *Catalog) Tables() []*Table {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]*Table, 0, len(c.tables))
	for _, t := range c.tables {
		out = append(out, t)
	}
	// A stable order keeps introspection output deterministic across runs,
	// which map iteration would not.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && strings.Compare(out[j-1].QualifiedName(), out[j].QualifiedName()) > 0; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
