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
	"cmp"
	"hash/maphash"
	"slices"
	"strings"
	"sync"

	"github.com/oxisto/lightsql/internal/ast"
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
	// PrimaryKey marks membership of the table's primary key. It is a hint for
	// introspection only; the constraint itself lives in Table.Constraints,
	// because a key over several columns is one constraint on the combination
	// rather than one per column.
	PrimaryKey bool
	// Default is the DEFAULT expression as written, or nil when the column has
	// none.
	//
	// The catalog holds the syntax rather than a bound expression because the
	// binder and the plan both sit above it; storing a plan expression here
	// would invert that. Each statement binds it afresh, which costs one bind
	// per statement rather than per row.
	Default ast.Expr
}

// ConstraintKind distinguishes a primary key from an ordinary unique
// constraint. They are enforced identically; only the reported name differs.
type ConstraintKind uint8

const (
	// PrimaryKeyConstraint is the table's primary key, of which there is at
	// most one. Its columns are implicitly NOT NULL.
	PrimaryKeyConstraint ConstraintKind = iota
	// UniqueConstraint is a UNIQUE constraint. Unlike a primary key, its
	// columns may be NULL.
	UniqueConstraint
)

func (k ConstraintKind) String() string {
	if k == PrimaryKeyConstraint {
		return "primary key"
	}
	return "unique"
}

// Constraint is a uniqueness requirement over one or more columns.
//
// Columns holds ordinals rather than names, and the constraint covers their
// combination. Modelling this as a flag on each column instead would make
// PRIMARY KEY (a, b) mean "a is unique and b is unique", which is a strictly
// stronger and wrong requirement.
type Constraint struct {
	// Name is the constraint name, either as written after CONSTRAINT or
	// derived in PostgreSQL's style, e.g. users_pkey or users_email_key.
	Name    string
	Kind    ConstraintKind
	Columns []int
}

// Check is a CHECK constraint: a predicate over a single row.
//
// It is kept apart from Constraint because the two are enforced by different
// machinery — a uniqueness constraint compares rows against each other, while a
// check evaluates an expression over one row and never looks at the others.
type Check struct {
	Name string
	// Expr is the predicate as written; see Column.Default for why the catalog
	// stores syntax rather than a bound expression.
	Expr ast.Expr
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
	// Constraints are the table's uniqueness constraints, checked on every
	// insert and update.
	Constraints []Constraint
	// Checks are the table's CHECK constraints, evaluated per row.
	Checks []Check
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
	// The rows already stored are unique among themselves, so only the new row
	// has to be checked against them.
	if err := t.checkUniqueRow(row); err != nil {
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
// The callback receives each row and returns its replacement, or nil to delete
// it. Two contracts hold, and only one of them is enforced here:
//
//   - Mutate builds a new outer slice and swaps it in at the end, so a reader
//     that already obtained a snapshot from Rows keeps iterating the old one.
//     A statement that fails partway therefore leaves the table untouched.
//   - The row passed to the callback is the stored row itself, not a copy.
//     A callback that wants to change a value must return a **new** slice;
//     writing through the argument would be visible to a reader holding a
//     snapshot and would defeat the guarantee above.
//
// Copying every row defensively would cost an allocation for the rows a
// statement does not even touch, so the second contract is left to the caller.
// TestMutateDoesNotDisturbSnapshots checks that the callers honour it.
//
// Together this is a weak stand-in for the MVCC snapshots planned for M2: it
// stops a concurrent scan from seeing a half-applied UPDATE, but it does not
// give a reader a stable view of a row it has already returned.
//
// The callback must not call back into the table.
func (t *Table) Mutate(fn func(row []types.Value) (replacement []types.Value, err error)) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	next := make([][]types.Value, 0, len(t.rows))
	for _, row := range t.rows {
		replacement, err := fn(row)
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
	if err := t.checkUnique(next); err != nil {
		return err
	}
	t.rows = next
	return nil
}

// checkRow enforces the per-row constraints. The caller must hold the write
// lock.
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

// checkUniqueRow verifies a candidate row against the rows already stored.
//
// This is the insert path, where everything already present is known to be
// unique among itself. It is a linear scan: there are no indexes yet, so the
// cost of an insert is proportional to the table. That is acceptable at the
// scale lightsql targets and is the first thing an index would fix.
//
// The caller must hold the write lock.
func (t *Table) checkUniqueRow(row []types.Value) error {
	for _, c := range t.Constraints {
		if anyNull(row, c.Columns) {
			continue // see checkUnique for why a NULL never conflicts
		}
		for _, existing := range t.rows {
			if !anyNull(existing, c.Columns) && keyEqual(row, existing, c.Columns) {
				return t.uniqueViolation(c, row)
			}
		}
	}
	return nil
}

// checkUnique verifies every uniqueness constraint over a whole row set.
//
// The update path validates the result as a whole rather than testing each
// changed row against the others, which is what PostgreSQL does by deferring the
// check to the end of the statement. That matters twice over: a row keeping its
// own value must not conflict with itself, and `UPDATE t SET a = a + 1` passes
// through states that collide but ends in a state that does not.
//
// The caller must hold the write lock.
func (t *Table) checkUnique(rows [][]types.Value) error {
	for _, c := range t.Constraints {
		// Rows are grouped by hash so the check is linear rather than
		// quadratic. The candidates in a bucket are still compared properly,
		// since a hash collision is not a duplicate.
		buckets := make(map[uint64][]int, len(rows))
		seed := maphash.MakeSeed()

		for i, row := range rows {
			// A NULL is never equal to anything, including another NULL, so a
			// row with a NULL in any key column cannot violate the constraint.
			// This is why UNIQUE permits any number of NULLs — the single most
			// surprising rule in this area, and one that using the grouping
			// form of equality would silently get wrong.
			if anyNull(row, c.Columns) {
				continue
			}

			var h maphash.Hash
			h.SetSeed(seed)
			for _, ord := range c.Columns {
				row[ord].Hash(&h)
			}
			sum := h.Sum64()

			for _, j := range buckets[sum] {
				if keyEqual(row, rows[j], c.Columns) {
					return t.uniqueViolation(c, row)
				}
			}
			buckets[sum] = append(buckets[sum], i)
		}
	}
	return nil
}

func anyNull(row []types.Value, cols []int) bool {
	for _, ord := range cols {
		if row[ord].IsNull() {
			return true
		}
	}
	return false
}

// keyEqual reports whether two rows agree on every key column. Only non-NULL
// rows reach here, so the grouping form of equality is the right one.
func keyEqual(a, b []types.Value, cols []int) bool {
	for _, ord := range cols {
		if !types.Equal(a[ord], b[ord]) {
			return false
		}
	}
	return true
}

// uniqueViolation builds the error, mirroring PostgreSQL's wording so that a
// parity test can compare the two.
func (t *Table) uniqueViolation(c Constraint, row []types.Value) error {
	names := make([]string, len(c.Columns))
	vals := make([]string, len(c.Columns))
	for i, ord := range c.Columns {
		names[i] = t.Columns[ord].Name
		vals[i] = row[ord].String()
	}
	return pgerr.Newf(pgerr.UniqueViolation,
		"duplicate key value violates unique constraint %q", c.Name).
		WithDetail("Key (%s)=(%s) already exists.",
			strings.Join(names, ", "), strings.Join(vals, ", "))
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
	slices.SortFunc(out, func(a, b *Table) int {
		return cmp.Compare(a.QualifiedName(), b.QualifiedName())
	})
	return out
}
