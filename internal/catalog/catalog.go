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
	"github.com/oxisto/lightsql/internal/storage"
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

// RefAction is what happens to a referencing row when the row it points at is
// deleted or has its key updated.
type RefAction uint8

const (
	// NoAction and Restrict both refuse the change while references remain.
	// PostgreSQL distinguishes them by when the check runs — NO ACTION can be
	// deferred to end of transaction — but lightsql has no deferred
	// constraints, so the two behave identically and the pair is kept only so
	// that a schema reads back as it was written.
	NoAction RefAction = iota
	Restrict
	// Cascade applies the change to the referencing rows too.
	Cascade
	// SetNull clears the referencing columns.
	SetNull
	// SetDefault puts the referencing columns back to their DEFAULT.
	SetDefault
)

var refActionNames = [...]string{
	NoAction: "NO ACTION", Restrict: "RESTRICT", Cascade: "CASCADE",
	SetNull: "SET NULL", SetDefault: "SET DEFAULT",
}

func (a RefAction) String() string { return refActionNames[a] }

// ForeignKey requires that this table's Columns match a row of Parent.
//
// Both sides are ordinals, resolved once when the table is created, so
// enforcement never looks a name up again.
type ForeignKey struct {
	Name    string
	Columns []int
	// Parent is the referenced table, and ParentCols the columns matched
	// against. They must carry a primary key or unique constraint, or a
	// reference could match several rows and the actions below would be
	// ambiguous.
	Parent     *Table
	ParentCols []int
	OnDelete   RefAction
	OnUpdate   RefAction
}

// Referencing is a foreign key pointing *at* a table, held by the table it
// points at.
//
// A delete or key update has to find every table that references the one being
// changed, and searching the whole catalog per statement would make the cost of
// a delete depend on how many unrelated tables exist.
type Referencing struct {
	Child *Table
	FK    *ForeignKey
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
	// ForeignKeys are the references this table makes to others.
	ForeignKeys []ForeignKey
	// referencedBy are the references other tables make to this one, so a
	// delete or key update can find them without scanning the catalog.
	//
	// It is unexported and reached only through the methods below, because the
	// binder appends to it while CREATE TABLE runs and the executor reads it
	// while a concurrent DELETE runs. A shared engine behind a connection pool
	// makes that a real data race, so the slice must never escape the lock.
	referencedBy []Referencing
	// refsMu guards referencedBy. It is separate from mu, which the per-insert
	// serial path takes, so a foreign-key read does not queue behind sequence
	// allocation.
	refsMu sync.RWMutex
	// defaultEval evaluates a DEFAULT expression; see SetDefaultEvaluator.
	defaultEval DefaultEvaluator
	// byName resolves a column name to its ordinal. Every reference below the
	// binder is an ordinal; this map is consulted during binding only.
	byName map[string]int

	// heap holds every version of every row. Storage is versioned rather than
	// overwritten so that a reader keeps seeing what was current when its
	// snapshot was taken, and so that rollback is one flag rather than an undo
	// log; see internal/storage.
	heap *storage.Heap

	// mu guards the sequence counters only. Row access is guarded by the heap.
	mu sync.Mutex
	// nextSerial holds the next value for each serial column, keyed by ordinal.
	// A sequence is per column rather than global so that truncating one table
	// cannot disturb another.
	//
	// Sequences deliberately sit outside the transaction: a rolled back INSERT
	// still consumes its value, exactly as in PostgreSQL, because handing the
	// same id to a later transaction would be worse than a gap.
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

// Insert adds a row on behalf of a transaction.
//
// The row must already be the right width and coerced to each column's type by
// the binder.
func (t *Table) Insert(tx *storage.Tx, row []types.Value) error {
	if err := t.checkRow(row); err != nil {
		return err
	}
	t.heap.Insert(tx.ID, row)
	return nil
}

// Scan returns the row versions visible to a transaction.
//
// Versions are returned rather than bare rows because a statement that goes on
// to update or delete a row needs to name the version it read; a row's values
// alone do not identify it.
func (t *Table) Scan(tx *storage.Tx) []*storage.Version {
	return t.heap.Scan(tx.Snapshot(), tx.ID)
}

// Rows returns the values visible to a transaction, for callers that only read.
func (t *Table) Rows(tx *storage.Tx) [][]types.Value {
	versions := t.Scan(tx)
	out := make([][]types.Value, len(versions))
	for i, v := range versions {
		out[i] = v.Vals
	}
	return out
}

// Update replaces a row version with new values.
//
// The old version is marked deleted by this transaction and a new one is
// written, so a reader on an older snapshot still sees the old values.
func (t *Table) Update(tx *storage.Tx, old *storage.Version, vals []types.Value) error {
	if err := t.checkRow(vals); err != nil {
		return err
	}
	if err := t.heap.Delete(tx.ID, old); err != nil {
		return err
	}
	t.heap.Insert(tx.ID, vals)
	return nil
}

// Delete removes a row version.
func (t *Table) Delete(tx *storage.Tx, v *storage.Version) error {
	return t.heap.Delete(tx.ID, v)
}

// EvalDefault produces a column's DEFAULT value.
//
// This is for SET DEFAULT, which needs the value outside the binder's usual
// path: a referential action has no statement of its own to bind against. The
// expression is bound and evaluated here in an empty scope, exactly as an
// omitted column in an INSERT would be.
func (t *Table) EvalDefault(ordinal int) (types.Value, error) {
	col := t.Columns[ordinal]
	if col.Default == nil {
		return types.Null(), nil
	}
	if t.defaultEval == nil {
		return types.Null(), pgerr.Newf(pgerr.InternalError,
			"no default evaluator installed for %q", t.Name)
	}
	return t.defaultEval(col.Default, col.Type.Kind)
}

// SetDefaultEvaluator installs the function that turns a written DEFAULT
// expression into a value.
//
// The catalog stores defaults as syntax and cannot evaluate them itself — the
// binder and executor both sit above it. Rather than invert that dependency,
// the engine hands the catalog a closure at startup.
func (c *Catalog) SetDefaultEvaluator(fn DefaultEvaluator) { c.defaultEval = fn }

// DefaultEvaluator binds and evaluates a DEFAULT expression for a column type.
type DefaultEvaluator func(expr ast.Expr, want types.Kind) (types.Value, error)

// CheckForeignKeys verifies that every row this transaction can see satisfies
// the table's outgoing references.
//
// It runs once at the end of a statement, like the uniqueness check, so a
// statement that temporarily points at a row it is about to insert is judged on
// where it ends up rather than on the order its rows happened to be written.
func (t *Table) CheckForeignKeys(tx *storage.Tx) error {
	if len(t.ForeignKeys) == 0 {
		return nil
	}
	for _, row := range t.Rows(tx) {
		for i := range t.ForeignKeys {
			if err := t.checkReference(tx, &t.ForeignKeys[i], row); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkReference verifies one row against one foreign key.
func (t *Table) checkReference(tx *storage.Tx, fk *ForeignKey, row []types.Value) error {
	// A NULL anywhere in the key satisfies the constraint. This is SQL's MATCH
	// SIMPLE, the default: a reference that is not fully specified does not
	// point anywhere, so there is nothing for it to fail to match.
	if anyNull(row, fk.Columns) {
		return nil
	}
	if parentHasKey(tx, fk, row) {
		return nil
	}

	names := make([]string, len(fk.Columns))
	vals := make([]string, len(fk.Columns))
	for i, ord := range fk.Columns {
		names[i] = t.Columns[ord].Name
		vals[i] = row[ord].String()
	}
	return pgerr.Newf(pgerr.ForeignKeyViolation,
		"insert or update on table %q violates foreign key constraint %q", t.Name, fk.Name).
		WithDetail("Key (%s)=(%s) is not present in table %q.",
			strings.Join(names, ", "), strings.Join(vals, ", "), fk.Parent.Name)
}

// parentHasKey reports whether the referenced table holds a row matching the
// child's key, as seen by this transaction.
//
// It is a scan of the parent per row checked. There are no indexes yet, so the
// cost of an insert is proportional to the parent table; an index on the
// referenced columns is the first thing that would fix it.
func parentHasKey(tx *storage.Tx, fk *ForeignKey, child []types.Value) bool {
	for _, prow := range fk.Parent.Rows(tx) {
		if matchesKey(child, fk.Columns, prow, fk.ParentCols) {
			return true
		}
	}
	return false
}

// matchesKey reports whether two rows agree across a pair of column lists.
func matchesKey(a []types.Value, aCols []int, b []types.Value, bCols []int) bool {
	for i, ord := range aCols {
		if !types.Equal(a[ord], b[bCols[i]]) {
			return false
		}
	}
	return true
}

// ChildrenOf returns the rows of referencing tables that point at the given
// parent row, grouped by the reference that found them.
func (t *Table) ChildrenOf(tx *storage.Tx, parent []types.Value) []ChildRows {
	t.refsMu.RLock()
	defer t.refsMu.RUnlock()

	var out []ChildRows
	for _, ref := range t.referencedBy {
		var rows []*storage.Version
		for _, v := range ref.Child.Scan(tx) {
			if anyNull(v.Vals, ref.FK.Columns) {
				continue // an unspecified reference points at nothing
			}
			if matchesKey(v.Vals, ref.FK.Columns, parent, ref.FK.ParentCols) {
				rows = append(rows, v)
			}
		}
		if len(rows) > 0 {
			out = append(out, ChildRows{Ref: ref, Rows: rows})
		}
	}
	return out
}

// AddReferencing records an incoming foreign key. The binder calls it while
// binding the child's CREATE TABLE, which can run concurrently with a statement
// on the parent.
func (t *Table) AddReferencing(r Referencing) {
	t.refsMu.Lock()
	defer t.refsMu.Unlock()
	t.referencedBy = append(t.referencedBy, r)
}

// IsReferenced reports whether any table points at this one, so a delete can
// skip the referential-action machinery entirely.
func (t *Table) IsReferenced() bool {
	t.refsMu.RLock()
	defer t.refsMu.RUnlock()
	return len(t.referencedBy) > 0
}

// ChildRows are the referencing rows found for one incoming foreign key.
type ChildRows struct {
	Ref  Referencing
	Rows []*storage.Version
}

// CheckConstraints validates the table's uniqueness constraints over the rows
// the transaction can now see.
//
// It runs once at the end of a statement rather than per row, which is what
// PostgreSQL does by deferring the check. That matters twice over: a row keeping
// its own value must not conflict with itself, and `UPDATE t SET a = a + 1`
// passes through states that collide but ends in one that does not.
func (t *Table) CheckConstraints(tx *storage.Tx) error {
	if len(t.Constraints) == 0 {
		return nil
	}
	return t.checkUnique(t.Rows(tx))
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
	// mgr is handed to each table's heap so that visibility is judged
	// consistently across the whole instance.
	mgr *storage.TxManager
	// defaultEval is handed to each table so SET DEFAULT can produce a value.
	defaultEval DefaultEvaluator

	mu     sync.RWMutex
	tables map[string]*Table
}

// New returns an empty catalog whose tables share one transaction manager.
func New(mgr *storage.TxManager) *Catalog {
	return &Catalog{mgr: mgr, tables: make(map[string]*Table)}
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
	t.heap = storage.NewHeap(c.mgr)
	t.defaultEval = c.defaultEval

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

	// Register the incoming references only now, with the table complete and in
	// the catalog. Doing it during binding published a half-built table — one
	// whose heap was not yet assigned — to any statement already walking the
	// parent's children. Taking the address of the slice element rather than of
	// a local also means the parent and the child share one foreign key, so a
	// later change to it cannot be seen by only one of them.
	for i := range t.ForeignKeys {
		fk := &t.ForeignKeys[i]
		fk.Parent.AddReferencing(Referencing{Child: t, FK: fk})
	}
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

// QualifiedName is a table name as written, before the default schema is
// applied.
type QualifiedName struct {
	Schema, Name string
}

// DropTable removes tables from the catalog.
//
// The whole set goes at once rather than one at a time, because a reference
// between two of the named tables is not a reason to refuse: `DROP TABLE parent,
// child` is legal even though dropping the parent on its own would not be.
//
// A table still referenced from outside the set is refused, which is SQL's
// RESTRICT default. Dropping one regardless would leave the referencing table
// enforcing a constraint against a relation nobody can look up.
func (c *Catalog) DropTable(names []QualifiedName, ifExists bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	going := make(map[string]*Table, len(names))
	for _, n := range names {
		schema := n.Schema
		if schema == "" {
			schema = DefaultSchema
		}
		t, ok := c.tables[key(schema, n.Name)]
		if !ok {
			if ifExists {
				continue
			}
			return pgerr.Newf(pgerr.UndefinedTable, "relation %q does not exist", n.Name)
		}
		going[key(schema, n.Name)] = t
	}

	for _, t := range going {
		if dep := t.referencedFromOutside(going); dep != "" {
			return pgerr.Newf(pgerr.ForeignKeyViolation,
				"cannot drop table %q because table %q depends on it", t.Name, dep)
		}
	}

	for k, t := range going {
		// Unregistering the table's own references matters as much as removing
		// it. A parent keeps a list of the children pointing at it, and leaving
		// a dropped child there would make a later DELETE from the parent
		// enforce a constraint against a table nobody can see.
		t.detach()
		delete(c.tables, k)
	}
	return nil
}

// referencedFromOutside names a table outside the given set that references t,
// or returns the empty string when none does.
func (t *Table) referencedFromOutside(going map[string]*Table) string {
	t.refsMu.RLock()
	defer t.refsMu.RUnlock()

	for _, ref := range t.referencedBy {
		if _, alsoGoing := going[key(ref.Child.Schema, ref.Child.Name)]; !alsoGoing {
			return ref.Child.Name
		}
	}
	return ""
}

// detach removes t from the referencedBy list of every table it points at.
func (t *Table) detach() {
	for i := range t.ForeignKeys {
		parent := t.ForeignKeys[i].Parent
		// A self-reference lives on the table being dropped, so there is
		// nothing outside it to clean up.
		if parent == nil || parent == t {
			continue
		}
		parent.removeReferencing(t)
	}
}

// removeReferencing drops every reference made by child.
func (t *Table) removeReferencing(child *Table) {
	t.refsMu.Lock()
	defer t.refsMu.Unlock()

	t.referencedBy = slices.DeleteFunc(t.referencedBy, func(r Referencing) bool {
		return r.Child == child
	})
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
