// Package binder resolves a parsed statement against the catalog, producing a
// plan.
//
// This is where SQL's name and type rules live: column references become
// ordinals, `*` is expanded, literals are converted to typed values, types are
// checked and promoted, and anything the dialect forbids is rejected with a
// SQLSTATE and a position. Everything downstream can then assume its input is
// well formed.
//
// Having this stage at all is what keeps the executor small. Without it, name
// resolution and type coercion end up interleaved with row processing, where
// they run once per row instead of once per statement and where errors surface
// halfway through a result set.
package binder

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/token"
	"github.com/oxisto/lightsql/internal/types"
)

// Binder resolves statements against a catalog.
type Binder struct {
	cat *catalog.Catalog
}

// New returns a Binder over the given catalog.
func New(cat *catalog.Catalog) *Binder { return &Binder{cat: cat} }

// scope is the set of columns visible to an expression, together with where each
// one lives in the row being processed.
type scope struct {
	cols []scopeColumn
	// agg is set while binding the parts of a SELECT that see the grouped row
	// rather than an input row. When it is nil, an aggregate call is an error.
	agg *aggContext
	// clause names the part of the statement being bound, for error messages
	// that need to say where an aggregate was misplaced.
	clause string
	// b is the binder, needed only to resolve a subquery in expression
	// position against the catalog. It is deliberately left nil in the scopes
	// where SQL forbids a subquery -- a CHECK constraint and a DEFAULT
	// expression -- so that rejecting one there is structural rather than an
	// extra test somebody has to remember to write.
	b *Binder
}

type scopeColumn struct {
	// table is the name the column may be qualified with: the table's alias if
	// it has one, otherwise its name.
	table   string
	name    string
	ordinal int
	typ     catalog.Type
	// hidden marks the right-hand copy of a column merged by JOIN ... USING.
	// The pair is one column as far as the query is concerned, so the copy must
	// not make an unqualified reference ambiguous and must not appear twice in
	// SELECT *. It stays in the scope, rather than being removed, because it is
	// still addressable as u.id and its ordinal still describes the joined row.
	hidden bool
}

// addTable brings every column of a table into scope under the given qualifier.
// The qualifier is the alias when one was written, and only the alias:
// PostgreSQL hides the original name once a table is aliased.
// The ordinal recorded is the column's position in the row the executor will
// build, not its position within its own table. For a single table those are
// the same; for a join the right side is offset by the width of the left, which
// is exactly what appending to a shared scope produces.
func (s *scope) addTable(t *catalog.Table, qualifier string) {
	base := len(s.cols)
	for i, c := range t.Columns {
		s.cols = append(s.cols, scopeColumn{
			table: qualifier, name: c.Name, ordinal: base + i, typ: c.Type,
		})
	}
}

// addColumns brings a subplan's output columns into scope under the given
// qualifier, the way addTable does for a base table. A derived table's columns
// are named by its select list rather than by the catalog, which is why this
// takes the plan's result columns instead of a *catalog.Table.
func (s *scope) addColumns(cols []plan.ResultColumn, qualifier string) {
	base := len(s.cols)
	for i, c := range cols {
		s.cols = append(s.cols, scopeColumn{
			table: qualifier, name: c.Name, ordinal: base + i, typ: c.Type,
		})
	}
}

// visible reports whether an unqualified reference may resolve to this column.
func (c *scopeColumn) visible(qualified bool) bool { return qualified || !c.hidden }

// resolve finds a column by an optionally qualified name. An unqualified name
// that matches more than one table is ambiguous, which SQL rejects rather than
// silently picking one.
func (s *scope) resolve(ref *ast.ColumnRef) (scopeColumn, error) {
	var found scopeColumn
	n := 0
	qualified := !ref.Table.IsEmpty()
	for _, c := range s.cols {
		if c.name != ref.Column.Name {
			continue
		}
		if qualified && c.table != ref.Table.Name {
			continue
		}
		if !c.visible(qualified) {
			continue
		}
		found = c
		n++
	}
	switch n {
	case 0:
		return scopeColumn{}, pgerr.Newf(pgerr.UndefinedColumn,
			"column %q does not exist", ref.String()).At(ref.Pos())
	case 1:
		return found, nil
	default:
		return scopeColumn{}, pgerr.Newf(pgerr.AmbiguousColumn,
			"column reference %q is ambiguous", ref.String()).At(ref.Pos())
	}
}

// Bind resolves a statement.
func (b *Binder) Bind(stmt ast.Stmt) (plan.Stmt, error) {
	switch s := stmt.(type) {
	case *ast.CreateTableStmt:
		return b.bindCreateTable(s)
	case *ast.DropTableStmt:
		return bindDropTable(s)
	case *ast.AlterTableStmt:
		return bindAlterTable(s)
	case *ast.CreateIndexStmt:
		return b.bindCreateIndex(s)
	case *ast.DropIndexStmt:
		return bindDropIndex(s)
	case *ast.InsertStmt:
		return b.bindInsert(s)
	case *ast.UpdateStmt:
		return b.bindUpdate(s)
	case *ast.DeleteStmt:
		return b.bindDelete(s)
	case *ast.SelectStmt:
		return b.bindSelect(s)
	default:
		return nil, pgerr.Newf(pgerr.FeatureNotSupported,
			"statement type %T is not supported yet", stmt).At(stmt.Pos())
	}
}

// bindDropTable resolves DROP TABLE.
//
// The tables are not looked up here. Existence is decided by the catalog, under
// the lock that also performs the drop, so that IF EXISTS and a concurrent drop
// cannot disagree between the check and the act.
func bindDropTable(s *ast.DropTableStmt) (plan.Stmt, error) {
	// CASCADE parses but is refused. In PostgreSQL it drops the dependent
	// objects, which for a table means removing the foreign key from every
	// surviving child -- and a child's constraints are aliased by the parent's
	// referencedBy list, so removing one moves the elements those pointers
	// address. Doing it here would leave foreign key enforcement reading the
	// wrong constraint, which is a worse outcome than saying no.
	if s.Cascade {
		return nil, pgerr.New(pgerr.FeatureNotSupported,
			"DROP TABLE ... CASCADE is not supported yet; drop the referencing table first").At(s.Pos())
	}

	out := &plan.DropTable{IfExists: s.IfExists}
	seen := make(map[string]bool, len(s.Tables))
	for _, t := range s.Tables {
		q := catalog.QualifiedName{Schema: t.Schema.Name, Name: t.Name.Name}
		// PostgreSQL rejects a repeated name rather than dropping it twice,
		// where the second attempt would report a table that is missing only
		// because the statement itself removed it.
		if seen[q.Schema+"."+q.Name] {
			return nil, pgerr.Newf(pgerr.DuplicateTable,
				"table %q specified more than once", t.Name.Name).At(t.Pos())
		}
		seen[q.Schema+"."+q.Name] = true
		out.Names = append(out.Names, q)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// CREATE TABLE
// ---------------------------------------------------------------------------

func (b *Binder) bindCreateTable(s *ast.CreateTableStmt) (plan.Stmt, error) {
	t := &catalog.Table{
		Schema: s.Table.Schema.Name,
		Name:   s.Table.Name.Name,
	}
	// Constraints are collected while walking, then applied once every column
	// ordinal is known: a table-level key may name a column declared later.
	var keys []keySpec
	var checks []catalog.Check
	var refs []refSpec

	for _, cd := range s.Columns {
		typ, err := catalog.ResolveType(cd.Type.Name, cd.Type.Mods)
		if err != nil {
			// The parser knows where the type was written; the resolver does not.
			return nil, at(err, cd.Type.Pos())
		}
		col := catalog.Column{Name: cd.Name.Name, Type: typ}

		// A column-level constraint covers exactly that column, so its ordinal
		// is the one about to be appended.
		ordinal := len(t.Columns)
		for _, c := range cd.Constraints {
			switch c.Kind {
			case ast.ConstraintNotNull:
				col.NotNull = true
			case ast.ConstraintPrimaryKey:
				// PRIMARY KEY implies NOT NULL, as in PostgreSQL.
				col.PrimaryKey = true
				col.NotNull = true
				keys = append(keys, keySpec{
					name: c.Name.Name, kind: catalog.PrimaryKeyConstraint,
					cols: []int{ordinal}, pos: c.Pos(),
				})
			case ast.ConstraintUnique:
				keys = append(keys, keySpec{
					name: c.Name.Name, kind: catalog.UniqueConstraint,
					cols: []int{ordinal}, pos: c.Pos(),
				})
			case ast.ConstraintDefault:
				// Bound now only to reject a bad expression at CREATE TABLE
				// rather than at the first INSERT; the catalog keeps the syntax.
				if _, err := b.bindDefault(c.Expr, typ.Kind); err != nil {
					return nil, err
				}
				col.Default = c.Expr
			case ast.ConstraintCheck:
				checks = append(checks, catalog.Check{Name: c.Name.Name, Expr: c.Expr})
			case ast.ConstraintReferences:
				refs = append(refs, refSpec{
					name: c.Name.Name, cols: []int{ordinal}, ref: c.Ref, pos: c.Pos(),
				})
			case ast.ConstraintNull:
				// Explicit NULL is the default and carries no information.
			default:
				return nil, pgerr.Newf(pgerr.FeatureNotSupported,
					"%s constraints are not supported yet", c.Kind).At(c.Pos())
			}
		}
		t.Columns = append(t.Columns, col)
	}

	for _, tc := range s.Constraints {
		var kind catalog.ConstraintKind
		switch tc.Kind {
		case ast.ConstraintPrimaryKey:
			kind = catalog.PrimaryKeyConstraint
		case ast.ConstraintUnique:
			kind = catalog.UniqueConstraint
		case ast.ConstraintCheck:
			checks = append(checks, catalog.Check{Name: tc.Name.Name, Expr: tc.Expr})
			continue
		case ast.ConstraintReferences:
			cols, err := columnOrdinals(t, tc.Columns)
			if err != nil {
				return nil, err
			}
			refs = append(refs, refSpec{name: tc.Name.Name, cols: cols, ref: tc.Ref, pos: tc.Pos()})
			continue
		default:
			return nil, pgerr.Newf(pgerr.FeatureNotSupported,
				"table-level %s constraints are not supported yet", tc.Kind).At(tc.Pos())
		}

		cols, err := columnOrdinals(t, tc.Columns)
		if err != nil {
			return nil, err
		}
		keys = append(keys, keySpec{name: tc.Name.Name, kind: kind, cols: cols, pos: tc.Pos()})
	}

	if err := applyKeys(t, keys); err != nil {
		return nil, err
	}

	// Checks are bound once here so that a predicate naming an unknown column
	// fails at CREATE TABLE rather than at the first INSERT.
	sc := &scope{}
	sc.addTable(t, t.Name)

	// Names already spoken for, so a derived name cannot collide with one the
	// statement wrote explicitly.
	taken := make(map[string]bool, len(t.Constraints)+len(checks))
	for _, c := range t.Constraints {
		taken[c.Name] = true
	}
	for _, c := range checks {
		if c.Name != "" {
			taken[c.Name] = true
		}
	}
	for i, c := range checks {
		if _, err := b.bindCheck(c, sc); err != nil {
			return nil, err
		}
		if c.Name == "" {
			checks[i].Name = uniqueCheckName(t.Name, taken)
			taken[checks[i].Name] = true
		}
	}
	t.Checks = checks

	// References resolve last: the referenced table must already exist, and
	// this table's own columns must already be known.
	for _, r := range refs {
		fk, err := b.resolveForeignKey(t, r)
		if err != nil {
			return nil, err
		}
		t.ForeignKeys = append(t.ForeignKeys, fk)
	}

	return &plan.CreateTable{Table: t, IfNotExists: s.IfNotExists}, nil
}

// uniqueCheckName derives a name for an unnamed CHECK, following PostgreSQL:
// the first is <table>_check, then <table>_check1, <table>_check2 and so on.
//
// Numbering matters because a violation reports the constraint name, and a
// table with two unnamed checks would otherwise attribute both to the same one.
func uniqueCheckName(table string, taken map[string]bool) string {
	base := table + "_check"
	if !taken[base] {
		return base
	}
	for i := 1; ; i++ {
		name := base + strconv.Itoa(i)
		if !taken[name] {
			return name
		}
	}
}

// bindDefault binds a DEFAULT expression and coerces it to the column's type.
//
// A default is evaluated in an empty scope: it may not reference any column,
// including the one it belongs to, because there is no row to read when the
// value is being produced.
func (b *Binder) bindDefault(e ast.Expr, want types.Kind) (plan.Expr, error) {
	bound, err := bindExpr(e, &scope{})
	if err != nil {
		return nil, err
	}
	return coerce(bound, want, e.Pos())
}

// EvalConstDefault binds a DEFAULT expression and evaluates it.
//
// Defaults are constant expressions bound in an empty scope, so this needs no
// row and no arguments. It exists for referential SET DEFAULT, which has no
// statement of its own to bind against.
func (b *Binder) EvalConstDefault(e ast.Expr, want types.Kind) (types.Value, error) {
	bound, err := b.bindDefault(e, want)
	if err != nil {
		return types.Value{}, err
	}
	c, ok := bound.(*plan.Const)
	if !ok {
		return types.Value{}, pgerr.Newf(pgerr.FeatureNotSupported,
			"only a constant DEFAULT can be applied by a referential action")
	}
	return c.Val, nil
}

// bindCheck binds a CHECK predicate against the table's own columns.
func (b *Binder) bindCheck(c catalog.Check, sc *scope) (plan.Expr, error) {
	pred, err := bindExpr(c.Expr, sc)
	if err != nil {
		return nil, err
	}
	if pred.Type() != types.KindBool && pred.Type() != types.KindNull {
		return nil, pgerr.Newf(pgerr.DatatypeMismatch,
			"argument of CHECK must be boolean, not %s", pred.Type()).At(c.Expr.Pos())
	}
	return pred, nil
}

// bindChecks binds every CHECK on a table, for a statement that writes rows.
func (b *Binder) bindChecks(t *catalog.Table) ([]plan.Check, error) {
	if len(t.Checks) == 0 {
		return nil, nil
	}
	sc := &scope{}
	sc.addTable(t, t.Name)

	out := make([]plan.Check, len(t.Checks))
	for i, c := range t.Checks {
		pred, err := b.bindCheck(c, sc)
		if err != nil {
			return nil, err
		}
		out[i] = plan.Check{Name: c.Name, Pred: pred}
	}
	return out, nil
}

// keySpec is a uniqueness constraint collected while walking the statement,
// before the column ordinals it names are all known.
type keySpec struct {
	name string
	kind catalog.ConstraintKind
	cols []int
	pos  token.Pos
}

// applyKeys turns the collected constraints into catalog entries, rejecting a
// second primary key and naming any that were written without one.
func applyKeys(t *catalog.Table, keys []keySpec) error {
	seenPK := false
	for _, k := range keys {
		if k.kind == catalog.PrimaryKeyConstraint {
			if seenPK {
				return pgerr.Newf(pgerr.SyntaxError,
					"multiple primary keys for table %q are not allowed", t.Name).At(k.pos)
			}
			seenPK = true
			// A primary key's columns are NOT NULL whether or not that was
			// written, which is what makes the NULL rule below moot for them.
			for _, ord := range k.cols {
				t.Columns[ord].PrimaryKey = true
				t.Columns[ord].NotNull = true
			}
		}

		name := k.name
		if name == "" {
			name = derivedKeyName(t, k)
		}
		t.Constraints = append(t.Constraints, catalog.Constraint{
			Name: name, Kind: k.kind, Columns: k.cols,
		})
	}
	return nil
}

// derivedKeyName names an unnamed constraint the way PostgreSQL does, so that
// the name in a violation message is the one a user would recognise:
// users_pkey for a primary key, users_email_key for a unique constraint.
func derivedKeyName(t *catalog.Table, k keySpec) string {
	if k.kind == catalog.PrimaryKeyConstraint {
		return t.Name + "_pkey"
	}
	parts := make([]string, 0, len(k.cols)+2)
	parts = append(parts, t.Name)
	for _, ord := range k.cols {
		parts = append(parts, t.Columns[ord].Name)
	}
	parts = append(parts, "key")
	return strings.Join(parts, "_")
}

// refSpec is a foreign key collected while walking a CREATE TABLE, before the
// referenced table has been looked up.
type refSpec struct {
	name string
	cols []int
	ref  *ast.ForeignKeyRef
	pos  token.Pos
}

// resolveForeignKey turns a written REFERENCES clause into a catalog entry,
// and registers it on the referenced table so a later delete can find it.
func (b *Binder) resolveForeignKey(t *catalog.Table, r refSpec) (catalog.ForeignKey, error) {
	parent, err := b.referencedTable(t, r)
	if err != nil {
		return catalog.ForeignKey{}, err
	}

	parentCols, err := b.referencedColumns(parent, r)
	if err != nil {
		return catalog.ForeignKey{}, err
	}
	if len(parentCols) != len(r.cols) {
		return catalog.ForeignKey{}, pgerr.Newf(pgerr.SyntaxError,
			"number of referencing and referenced columns for foreign key disagree").At(r.pos)
	}

	// The referenced columns must be unique. Without that a child row could
	// match several parents, and CASCADE would have no single row to follow —
	// so PostgreSQL requires it, and so does this.
	if !hasUniqueKeyOver(parent, parentCols) {
		names := make([]string, len(parentCols))
		for i, ord := range parentCols {
			names[i] = parent.Columns[ord].Name
		}
		return catalog.ForeignKey{}, pgerr.Newf(pgerr.UndefinedObject,
			"there is no unique constraint matching given keys for referenced table %q",
			parent.Name).WithDetail("Columns (%s) must carry a primary key or unique constraint.",
			strings.Join(names, ", ")).At(r.pos)
	}

	// The types must match, or a comparison between them would be meaningless.
	for i, ord := range r.cols {
		if got, want := t.Columns[ord].Type.Kind, parent.Columns[parentCols[i]].Type.Kind; got != want {
			return catalog.ForeignKey{}, pgerr.Newf(pgerr.DatatypeMismatch,
				"foreign key constraint cannot be implemented: column %q is %s and referenced column %q is %s",
				t.Columns[ord].Name, got, parent.Columns[parentCols[i]].Name, want).At(r.pos)
		}
	}

	name := r.name
	if name == "" {
		name = t.Name + "_" + t.Columns[r.cols[0]].Name + "_fkey"
	}
	fk := catalog.ForeignKey{
		Name: name, Columns: r.cols,
		Parent: parent, ParentCols: parentCols,
		OnDelete: refActionOf(r.ref.OnDelete),
		OnUpdate: refActionOf(r.ref.OnUpdate),
	}
	// The reverse registration on the parent is deliberately not done here.
	// The binder runs before the table exists, so publishing the child to its
	// parent now would expose a table with no heap to a concurrent statement,
	// and would leave a dangling registration behind if CREATE TABLE went on to
	// fail. CreateTable does it once the table is complete.
	return fk, nil
}

// referencedTable resolves the target, allowing a table to reference itself
// even though it is not in the catalog yet.
func (b *Binder) referencedTable(t *catalog.Table, r refSpec) (*catalog.Table, error) {
	name := r.ref.Table
	schema := name.Schema.Name
	if schema == "" {
		schema = catalog.DefaultSchema
	}
	self := t.Schema
	if self == "" {
		self = catalog.DefaultSchema
	}
	// A self-reference is legal and common — a tree stored as parent_id — and
	// the table is still being built, so it cannot be looked up.
	if schema == self && name.Name.Name == t.Name {
		return t, nil
	}

	parent, err := b.cat.Lookup(schema, name.Name.Name)
	if err != nil {
		return nil, at(err, name.Pos())
	}
	return parent, nil
}

// referencedColumns resolves the target columns, defaulting to the referenced
// table's primary key when the clause names none.
func (b *Binder) referencedColumns(parent *catalog.Table, r refSpec) ([]int, error) {
	if len(r.ref.Columns) > 0 {
		return columnOrdinals(parent, r.ref.Columns)
	}
	for _, c := range parent.Constraints {
		if c.Kind == catalog.PrimaryKeyConstraint {
			return c.Columns, nil
		}
	}
	return nil, pgerr.Newf(pgerr.UndefinedObject,
		"there is no primary key for referenced table %q", parent.Name).At(r.pos)
}

// hasUniqueKeyOver reports whether a constraint covers exactly the given
// columns, in any order.
func hasUniqueKeyOver(t *catalog.Table, cols []int) bool {
	for _, c := range t.Constraints {
		if len(c.Columns) != len(cols) {
			continue
		}
		match := true
		for _, ord := range cols {
			if !slices.Contains(c.Columns, ord) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func refActionOf(a ast.RefAction) catalog.RefAction {
	switch a {
	case ast.Cascade:
		return catalog.Cascade
	case ast.SetNull:
		return catalog.SetNull
	case ast.SetDefault:
		return catalog.SetDefault
	case ast.Restrict:
		return catalog.Restrict
	default:
		return catalog.NoAction
	}
}

// columnOrdinals resolves a list of column names against a table under
// construction, rejecting unknown and repeated names.
func columnOrdinals(t *catalog.Table, names []ast.Name) ([]int, error) {
	cols := make([]int, 0, len(names))
	for _, name := range names {
		i := indexOfColumn(t.Columns, name.Name)
		if i < 0 {
			return nil, pgerr.Newf(pgerr.UndefinedColumn,
				"column %q named in key does not exist", name.Name).At(name.Pos())
		}
		if slices.Contains(cols, i) {
			return nil, pgerr.Newf(pgerr.DuplicateColumn,
				"column %q appears twice in key", name.Name).At(name.Pos())
		}
		cols = append(cols, i)
	}
	return cols, nil
}

func indexOfColumn(cols []catalog.Column, name string) int {
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// INSERT
// ---------------------------------------------------------------------------

func (b *Binder) bindInsert(s *ast.InsertStmt) (plan.Stmt, error) {
	t, err := b.cat.Lookup(s.Table.Schema.Name, s.Table.Name.Name)
	if err != nil {
		return nil, at(err, s.Table.Pos())
	}

	// Without an explicit column list, the targets are every column in
	// declaration order.
	targets := make([]int, 0, len(t.Columns))
	if len(s.Columns) == 0 {
		for i := range t.Columns {
			targets = append(targets, i)
		}
	} else {
		seen := make(map[int]bool, len(s.Columns))
		for _, name := range s.Columns {
			i := t.ColumnIndex(name.Name)
			if i < 0 {
				return nil, pgerr.Newf(pgerr.UndefinedColumn,
					"column %q of relation %q does not exist", name.Name, t.Name).At(name.Pos())
			}
			if seen[i] {
				return nil, pgerr.Newf(pgerr.DuplicateColumn,
					"column %q specified more than once", name.Name).At(name.Pos())
			}
			seen[i] = true
			targets = append(targets, i)
		}
	}

	ins := &plan.Insert{Table: t, Targets: targets}

	if s.Select != nil {
		if ins.Source, err = b.insertSource(s.Select, t, targets); err != nil {
			return nil, err
		}
	}

	for _, row := range s.Rows {
		if len(row) != len(targets) {
			return nil, pgerr.Newf(pgerr.SyntaxError,
				"INSERT has %d expressions but %d target columns",
				len(row), len(targets)).At(row[0].Pos())
		}
		bound := make([]plan.Expr, len(row))
		for i, e := range row {
			// Each value is bound in an empty scope: an inserted expression
			// cannot refer to a column of the row being built.
			be, err := bindExpr(e, &scope{b: b})
			if err != nil {
				return nil, err
			}
			if bound[i], err = coerce(be, t.Columns[targets[i]].Type.Kind, e.Pos()); err != nil {
				return nil, err
			}
		}
		ins.Rows = append(ins.Rows, bound)
	}

	// Columns the statement did not name are filled from their sequence or
	// their DEFAULT. A serial column wins, since SERIAL is itself shorthand for
	// a default drawn from a sequence.
	for i, col := range t.Columns {
		if slices.Contains(targets, i) {
			continue
		}
		switch {
		case col.Type.Serial:
			ins.Serials = append(ins.Serials, i)
		case col.Default != nil:
			d, err := b.bindDefault(col.Default, col.Type.Kind)
			if err != nil {
				return nil, err
			}
			if ins.Defaults == nil {
				ins.Defaults = make(map[int]plan.Expr)
			}
			ins.Defaults[i] = d
		}
	}

	if ins.Checks, err = b.bindChecks(t); err != nil {
		return nil, err
	}

	if s.OnConflict != nil {
		if ins.OnConflict, err = b.bindOnConflict(s.OnConflict, t); err != nil {
			return nil, err
		}
	}

	// RETURNING sees the row as stored, including generated serial values —
	// which is the whole point of `INSERT ... RETURNING id`.
	if len(s.Returning) > 0 {
		sc := &scope{b: b}
		sc.addTable(t, t.Name)
		if ins.Returning, err = b.bindReturning(s.Returning, sc); err != nil {
			return nil, err
		}
	}
	return ins, nil
}

// insertSource binds the SELECT of an INSERT ... SELECT and adapts its output
// to the target columns.
//
// The adaptation is a projection rather than something the executor does per
// row, and it converts each column with the same coerce the VALUES form uses.
// That is what keeps the two spellings of INSERT agreeing about types: without
// it, `INSERT ... VALUES ('7')` and `INSERT ... SELECT '7'` could disagree
// about whether a text literal may land in an integer column.
func (b *Binder) insertSource(sel *ast.SelectStmt, t *catalog.Table, targets []int) (plan.Node, error) {
	src, err := b.bindSelectNode(sel)
	if err != nil {
		return nil, err
	}

	cols := src.Result()
	if len(cols) != len(targets) {
		return nil, pgerr.Newf(pgerr.SyntaxError,
			"INSERT has %d expressions but %d target columns",
			len(cols), len(targets)).At(sel.Pos())
	}

	proj := &plan.Project{Input: src}
	for i, c := range cols {
		want := t.Columns[targets[i]].Type
		var e plan.Expr = &plan.Column{Ordinal: i, Kind: c.Type.Kind, Name: c.Name}
		if e, err = coerce(e, want.Kind, sel.Pos()); err != nil {
			return nil, err
		}
		proj.Exprs = append(proj.Exprs, e)
		proj.Cols = append(proj.Cols, plan.ResultColumn{Name: c.Name, Type: want})
	}
	return proj, nil
}

// ---------------------------------------------------------------------------
// UPDATE and DELETE
// ---------------------------------------------------------------------------

func (b *Binder) bindUpdate(s *ast.UpdateStmt) (plan.Stmt, error) {
	t, sc, err := b.bindTarget(s.Table, s.Alias)
	if err != nil {
		return nil, err
	}

	up := &plan.Update{Table: t}
	seen := make(map[int]bool, len(s.Assignments))
	for _, a := range s.Assignments {
		i := t.ColumnIndex(a.Column.Name)
		if i < 0 {
			return nil, pgerr.Newf(pgerr.UndefinedColumn,
				"column %q of relation %q does not exist", a.Column.Name, t.Name).At(a.Column.Pos())
		}
		// PostgreSQL rejects a repeated target rather than picking one, since
		// either choice would be arbitrary.
		if seen[i] {
			return nil, pgerr.Newf(pgerr.SyntaxError,
				"multiple assignments to same column %q", a.Column.Name).At(a.Column.Pos())
		}
		seen[i] = true

		// The right-hand side is bound in the table's scope, so an assignment
		// may read the row it is updating: SET n = n + 1 sees the old value.
		val, err := bindExpr(a.Value, sc)
		if err != nil {
			return nil, err
		}
		if val, err = coerce(val, t.Columns[i].Type.Kind, a.Value.Pos()); err != nil {
			return nil, err
		}
		up.Assignments = append(up.Assignments, plan.Assignment{Ordinal: i, Value: val})
	}

	if up.Where, err = b.bindPredicate(s.Where, sc, "WHERE"); err != nil {
		return nil, err
	}
	if up.Checks, err = b.bindChecks(t); err != nil {
		return nil, err
	}
	if up.Returning, err = b.bindReturning(s.Returning, sc); err != nil {
		return nil, err
	}
	return up, nil
}

func (b *Binder) bindDelete(s *ast.DeleteStmt) (plan.Stmt, error) {
	t, sc, err := b.bindTarget(s.Table, s.Alias)
	if err != nil {
		return nil, err
	}

	del := &plan.Delete{Table: t}
	if del.Where, err = b.bindPredicate(s.Where, sc, "WHERE"); err != nil {
		return nil, err
	}
	// RETURNING on a DELETE reports the rows as they were before removal, which
	// is the only thing it could usefully mean.
	if del.Returning, err = b.bindReturning(s.Returning, sc); err != nil {
		return nil, err
	}
	return del, nil
}

// bindTarget resolves the table a DML statement operates on and builds the scope
// its expressions see.
func (b *Binder) bindTarget(name *ast.TableName, alias ast.Name) (*catalog.Table, *scope, error) {
	t, err := b.cat.Lookup(name.Schema.Name, name.Name.Name)
	if err != nil {
		return nil, nil, at(err, name.Pos())
	}
	qualifier := t.Name
	if !alias.IsEmpty() {
		qualifier = alias.Name
	}
	sc := &scope{b: b}
	sc.addTable(t, qualifier)
	return t, sc, nil
}

// bindPredicate binds a clause that must yield a boolean.
func (b *Binder) bindPredicate(e ast.Expr, sc *scope, clause string) (plan.Expr, error) {
	if e == nil {
		return nil, nil
	}
	pred, err := bindExpr(e, sc)
	if err != nil {
		return nil, err
	}
	if pred.Type() != types.KindBool && pred.Type() != types.KindNull {
		return nil, pgerr.Newf(pgerr.DatatypeMismatch,
			"argument of %s must be boolean, not %s", clause, pred.Type()).At(e.Pos())
	}
	return pred, nil
}

// bindReturning binds a RETURNING list, which is a select list evaluated over
// the affected row.
func (b *Binder) bindReturning(items []ast.SelectItem, sc *scope) (*plan.Returning, error) {
	if len(items) == 0 {
		return nil, nil
	}
	proj, err := b.bindSelectItems(items, sc, nil)
	if err != nil {
		return nil, err
	}
	return &plan.Returning{Exprs: proj.Exprs, Cols: proj.Cols}, nil
}

// ---------------------------------------------------------------------------
// SELECT
// ---------------------------------------------------------------------------

// bindFrom resolves one FROM item, bringing its columns into sc and returning
// the node that produces them.
//
// It recurses, because a join's operands are themselves table expressions:
// `a JOIN b JOIN c` is a join whose left side is a join. Order matters — the
// left side must be added to the scope before the right, since that is what
// gives each column the ordinal it will have in the joined row.
func (b *Binder) bindFrom(te ast.TableExpr, sc *scope) (plan.Node, error) {
	switch te := te.(type) {
	case *ast.TableRef:
		t, err := b.cat.Lookup(te.Table.Schema.Name, te.Table.Name.Name)
		if err != nil {
			return nil, at(err, te.Pos())
		}
		// The qualifier is the alias when one was written, and only the alias:
		// PostgreSQL hides the original name once a table is aliased.
		qualifier := t.Name
		if !te.Alias.IsEmpty() {
			qualifier = te.Alias.Name
		}
		sc.addTable(t, qualifier)

		cols := make([]plan.ResultColumn, len(t.Columns))
		for i, c := range t.Columns {
			cols[i] = plan.ResultColumn{Name: c.Name, Type: c.Type}
		}
		return &plan.Scan{Table: t, Cols: cols}, nil

	case *ast.JoinExpr:
		return b.bindJoin(te, sc)

	case *ast.SubqueryRef:
		// The alias is checked before the body is bound, because it is a
		// structural requirement: without it the derived table's columns have
		// no name to be qualified by, and reporting a missing alias is more
		// use than reporting the first thing wrong inside a subquery that
		// could not have been referenced anyway.
		if te.Alias.IsEmpty() {
			return nil, pgerr.New(pgerr.SyntaxError,
				"subquery in FROM must have an alias").At(te.Pos())
		}
		// The body is bound in a scope of its own. Without LATERAL a derived
		// table cannot see the tables beside it in the outer FROM, and passing
		// the outer scope down would silently make every derived table lateral
		// — the reference would resolve, against an ordinal describing a row
		// the subplan never sees.
		node, err := b.bindSelectNode(te.Select)
		if err != nil {
			return nil, err
		}
		// No node of its own: a derived table is exactly the rows its body
		// produces, so the subplan is spliced straight in. The ordinals the
		// scope hands out already describe the row the outer plan builds,
		// because that is how a join concatenates its inputs.
		sc.addColumns(node.Result(), te.Alias.Name)
		return node, nil

	default:
		// Naming the construct rather than printing the Go type keeps the
		// message stable and meaningful to someone reading their own SQL.
		return nil, pgerr.New(pgerr.FeatureNotSupported,
			"this FROM item is not supported yet").At(te.Pos())
	}
}

func (b *Binder) bindJoin(j *ast.JoinExpr, sc *scope) (plan.Node, error) {
	left, err := b.bindFrom(j.Left, sc)
	if err != nil {
		return nil, err
	}
	// Everything already in scope belongs to the left side. Recording the
	// boundary here is what lets USING find the two copies of a name.
	boundary := len(sc.cols)

	right, err := b.bindFrom(j.Right, sc)
	if err != nil {
		return nil, err
	}

	node := &plan.Join{Left: left, Right: right, Type: j.Type}

	switch {
	case len(j.Using) > 0:
		if node.Pred, err = b.bindUsing(j, sc, boundary); err != nil {
			return nil, err
		}
	case j.On != nil:
		// The condition sees both sides, which is the whole point of binding it
		// after the right side has been added.
		if node.Pred, err = b.bindPredicate(j.On, sc, "JOIN/ON"); err != nil {
			return nil, err
		}
	case j.Type != ast.CrossJoin:
		return nil, pgerr.Newf(pgerr.SyntaxError,
			"%s JOIN requires ON or USING", j.Type).At(j.JoinPos)
	}

	// Cols describes the row the executor actually builds, so it keeps both
	// copies of a USING column. Ordinals index that row, and a join can itself
	// be the left side of another join, so narrowing this would put every
	// ordinal above it out by one. Hiding is a scope concern, applied where
	// names are resolved and where * is expanded.
	node.Cols = joinCols(left, right)
	return node, nil
}

// bindUsing turns USING (a, b) into the conjunction left.a = right.a AND
// left.b = right.b, and merges each pair into a single visible column.
func (b *Binder) bindUsing(j *ast.JoinExpr, sc *scope, boundary int) (plan.Expr, error) {
	var pred plan.Expr
	for _, name := range j.Using {
		l, err := sc.only(name.Name, 0, boundary, name.NamePos)
		if err != nil {
			return nil, err
		}
		r, err := sc.only(name.Name, boundary, len(sc.cols), name.NamePos)
		if err != nil {
			return nil, err
		}
		// The right-hand copy stops being independently visible: USING says the
		// two are one column, so an unqualified reference must not be ambiguous
		// and SELECT * must not show it twice.
		sc.cols[r].hidden = true

		eq := &plan.Binary{
			Op:   ast.OpEq,
			L:    &plan.Column{Ordinal: sc.cols[l].ordinal, Kind: sc.cols[l].typ.Kind, Name: name.Name},
			R:    &plan.Column{Ordinal: sc.cols[r].ordinal, Kind: sc.cols[r].typ.Kind, Name: name.Name},
			Kind: types.KindBool,
		}
		if pred == nil {
			pred = eq
			continue
		}
		pred = &plan.Binary{Op: ast.OpAnd, L: pred, R: eq, Kind: types.KindBool}
	}
	return pred, nil
}

// only finds the single column named name in sc.cols[lo:hi], for USING, which
// needs exactly one candidate on each side.
func (s *scope) only(name string, lo, hi int, pos token.Pos) (int, error) {
	found := -1
	for i := lo; i < hi; i++ {
		if s.cols[i].name != name || s.cols[i].hidden {
			continue
		}
		if found >= 0 {
			return 0, pgerr.Newf(pgerr.AmbiguousColumn,
				"common column name %q appears more than once", name).At(pos)
		}
		found = i
	}
	if found < 0 {
		return 0, pgerr.Newf(pgerr.UndefinedColumn,
			"column %q specified in USING clause does not exist in one of the tables", name).At(pos)
	}
	return found, nil
}

// joinCols is the concatenation of the two inputs' columns, which is exactly
// the row the join operator emits.
func joinCols(left, right plan.Node) []plan.ResultColumn {
	l, r := left.Result(), right.Result()
	out := make([]plan.ResultColumn, 0, len(l)+len(r))
	out = append(out, l...)
	return append(out, r...)
}

func (b *Binder) bindSelect(s *ast.SelectStmt) (plan.Stmt, error) {
	root, err := b.bindSelectNode(s)
	if err != nil {
		return nil, err
	}
	return &plan.Query{Root: root}, nil
}

// bindSelectNode binds a SELECT to the node that produces its rows.
//
// It is separate from bindSelect because a SELECT is not always a statement: a
// derived table is one in FROM position, and a subquery is one in expression
// position. Those need the node without the plan.Query wrapper, and binding
// them through the same function is what keeps a subquery's clause handling
// from drifting away from a top-level query's.
func (b *Binder) bindSelectNode(s *ast.SelectStmt) (plan.Node, error) {
	sc := &scope{b: b}
	// A SELECT without FROM is evaluated over one empty row, so every node
	// below always has an input and no consumer needs a nil special case.
	var node plan.Node = &plan.SingleRow{}

	for i, item := range s.From {
		n, err := b.bindFrom(item, sc)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			node = n
			continue
		}
		// A comma in FROM is an inner join with no condition, which is what
		// CROSS JOIN means, so the two spellings produce the same plan rather
		// than a second code path.
		node = &plan.Join{Left: node, Right: n, Type: ast.CrossJoin, Cols: joinCols(node, n)}
	}

	if s.Where != nil {
		// WHERE runs below grouping, so it sees input rows and an aggregate in
		// it has nothing to aggregate over. Naming the clause lets the error
		// say so rather than reporting a missing function.
		sc.clause = "WHERE"
		pred, err := b.bindPredicate(s.Where, sc, "WHERE")
		sc.clause = ""
		if err != nil {
			return nil, err
		}
		node = &plan.Filter{Input: node, Pred: pred}
	}

	// A query is grouped when it says GROUP BY, and also when it merely uses an
	// aggregate: SELECT count(*) FROM t has one group covering every row.
	grouped := len(s.GroupBy) > 0 || s.Having != nil || selectHasAggregate(s)
	if grouped {
		if err := b.beginGrouping(s, sc); err != nil {
			return nil, err
		}
	}

	// The select list is bound before ORDER BY so that a term naming an output
	// alias can reuse the item's already-bound expression. Its input is attached
	// afterwards, once any Sort has been slotted underneath.
	proj, err := b.bindSelectItems(s.Items, sc, nil)
	if err != nil {
		return nil, err
	}

	var having plan.Expr
	if s.Having != nil {
		sc.clause = "HAVING"
		if having, err = b.bindPredicate(s.Having, sc, "HAVING"); err != nil {
			return nil, err
		}
		sc.clause = ""
	}

	var sortKeys []plan.SortKey
	if len(s.OrderBy) > 0 {
		if sortKeys, err = b.bindOrderBy(s.OrderBy, sc, proj); err != nil {
			return nil, err
		}
	}

	// Bound here, with the other clauses above the input, because in a grouped
	// query DISTINCT ON may name an aggregate and the aggregate node is built
	// only once every such clause has contributed its calls.
	var distinctOn []plan.Expr
	for _, e := range s.DistinctOn {
		on, err := bindExpr(e, sc)
		if err != nil {
			return nil, err
		}
		distinctOn = append(distinctOn, on)
	}

	// Every expression above the grouping has now been bound, so the set of
	// aggregate calls is complete and the node can be built. Doing it here
	// rather than before binding is what lets ORDER BY count(*) contribute a
	// call that the select list never mentioned.
	if grouped {
		node = b.groupingNode(node, sc)
	}
	if having != nil {
		node = &plan.Filter{Input: node, Pred: having}
	}
	if sortKeys != nil {
		node = &plan.Sort{Input: node, Keys: sortKeys}
	}
	proj.Input = node

	var out plan.Node = proj
	if s.Distinct {
		// The output shape is the select list as written; anything appended
		// below is scaffolding for DISTINCT ON and is trimmed straight off.
		d := &plan.Distinct{Input: proj, Width: len(proj.Exprs), Cols: proj.Cols}
		for i, on := range distinctOn {
			proj.Exprs = append(proj.Exprs, on)
			proj.Cols = append(proj.Cols, plan.ResultColumn{
				Name: "", Type: catalog.Type{Kind: on.Type(), Name: on.Type().String()},
			})
			d.On = append(d.On, &plan.Column{Ordinal: d.Width + i, Kind: on.Type()})
		}
		out = d
	}
	if s.Limit != nil || s.Offset != nil {
		lim := &plan.Limit{Input: out}
		if lim.Count, err = bindRowCount(s.Limit, "LIMIT"); err != nil {
			return nil, err
		}
		if lim.Offset, err = bindRowCount(s.Offset, "OFFSET"); err != nil {
			return nil, err
		}
		out = lim
	}
	return out, nil
}

// bindOrderBy resolves the ORDER BY terms against the select list and the input
// scope.
//
// SQL allows a term to be written three ways, and they are tried in the order
// PostgreSQL uses:
//
//  1. An output column name or alias — `SELECT a AS x ... ORDER BY x`. Output
//     names win over input columns here, which is why this is checked first.
//  2. An ordinal position into the select list — `ORDER BY 1`. Only a bare
//     integer counts; `ORDER BY 1 + 1` is an expression that sorts every row by
//     the same constant, not a reference to column 2.
//  3. Any expression over the input — `SELECT a FROM t ORDER BY b` is valid even
//     though b is not selected.
//
// All three produce an expression in the input's scope, which is what lets Sort
// sit below Project.
func (b *Binder) bindOrderBy(items []ast.OrderByItem, sc *scope, proj *plan.Project) ([]plan.SortKey, error) {
	keys := make([]plan.SortKey, 0, len(items))

	for _, item := range items {
		expr, err := b.bindSortTerm(item.Expr, sc, proj)
		if err != nil {
			return nil, err
		}
		desc := item.Dir == ast.SortDesc
		keys = append(keys, plan.SortKey{
			Expr: expr,
			Desc: desc,
			// PostgreSQL treats NULL as larger than every other value, so the
			// default follows the direction: last for ASC, first for DESC.
			NullsFirst: nullsFirst(item.Nulls, desc),
		})
	}
	return keys, nil
}

func nullsFirst(order ast.NullsOrder, desc bool) bool {
	switch order {
	case ast.NullsFirst:
		return true
	case ast.NullsLast:
		return false
	default:
		return desc
	}
}

func (b *Binder) bindSortTerm(e ast.Expr, sc *scope, proj *plan.Project) (plan.Expr, error) {
	// An unqualified name matching an output column refers to it.
	if ref, ok := e.(*ast.ColumnRef); ok && ref.Table.IsEmpty() && ref.Schema.IsEmpty() {
		for i, col := range proj.Cols {
			if col.Name == ref.Column.Name {
				return proj.Exprs[i], nil
			}
		}
	}

	// A bare integer is a position in the select list.
	if lit, ok := e.(*ast.Literal); ok && lit.Kind == ast.LitNumber {
		n, err := strconv.Atoi(lit.Val)
		if err == nil {
			if n < 1 || n > len(proj.Exprs) {
				return nil, pgerr.Newf(pgerr.SyntaxError,
					"ORDER BY position %d is not in the select list", n).At(lit.Pos())
			}
			return proj.Exprs[n-1], nil
		}
	}

	return bindExpr(e, sc)
}

// bindSelectItems builds the projection, expanding any star into the columns
// currently in scope.
func (b *Binder) bindSelectItems(items []ast.SelectItem, sc *scope, input plan.Node) (*plan.Project, error) {
	proj := &plan.Project{Input: input}

	for _, item := range items {
		if star, ok := item.Expr.(*ast.Star); ok {
			if len(sc.cols) == 0 {
				return nil, pgerr.New(pgerr.SyntaxError,
					"SELECT * with no tables specified is not valid").At(star.Pos())
			}
			matched := false
			for _, c := range sc.cols {
				if !star.Table.IsEmpty() && c.table != star.Table.Name {
					continue
				}
				// A column merged by USING is one column, so * shows it once.
				// It stays reachable as u.id, hence the qualified exception.
				if !c.visible(!star.Table.IsEmpty()) {
					continue
				}
				matched = true
				proj.Exprs = append(proj.Exprs,
					&plan.Column{Ordinal: c.ordinal, Kind: c.typ.Kind, Name: c.name})
				proj.Cols = append(proj.Cols, plan.ResultColumn{Name: c.name, Type: c.typ})
			}
			if !matched {
				return nil, pgerr.Newf(pgerr.UndefinedTable,
					"table name %q not found in FROM clause", star.Table.Name).At(star.Pos())
			}
			continue
		}

		e, err := bindExpr(item.Expr, sc)
		if err != nil {
			return nil, err
		}
		proj.Exprs = append(proj.Exprs, e)
		proj.Cols = append(proj.Cols, plan.ResultColumn{
			Name: outputName(item, e),
			Type: resultType(e, sc),
		})
	}
	return proj, nil
}

// resultType reports the declared type of a projected expression.
//
// A bare column reference keeps the type it was declared with, so that
// DatabaseTypeName reports "character varying" rather than the "text" the engine
// happens to store it as — ORMs read that name when mapping a result set. A
// computed expression has no declared type, so it falls back to its kind.
func resultType(e plan.Expr, sc *scope) catalog.Type {
	if c, ok := e.(*plan.Column); ok {
		// A grouped query's ordinals index the grouped row, so the input scope
		// is the wrong table to look them up in — and it would not fail, it
		// would return an unrelated column's type.
		if sc.agg != nil {
			if t, ok := sc.agg.resultType(c.Ordinal); ok {
				return t
			}
		} else {
			for _, sc := range sc.cols {
				if sc.ordinal == c.Ordinal {
					return sc.typ
				}
			}
		}
	}
	return catalog.Type{Kind: e.Type(), Name: e.Type().String()}
}

// outputName picks the label a result column is reported under: the alias if
// given, the column name for a bare reference, and PostgreSQL's fallback of
// "?column?" otherwise.
func outputName(item ast.SelectItem, bound plan.Expr) string {
	if !item.Alias.IsEmpty() {
		return item.Alias.Name
	}
	if c, ok := bound.(*plan.Column); ok {
		return c.Name
	}
	return "?column?"
}

// bindRowCount binds a LIMIT or OFFSET operand, which must be an integer.
func bindRowCount(e ast.Expr, clause string) (plan.Expr, error) {
	if e == nil {
		return nil, nil
	}
	bound, err := bindExpr(e, &scope{})
	if err != nil {
		return nil, err
	}
	switch bound.Type() {
	case types.KindInt, types.KindNull:
		return bound, nil
	default:
		return nil, pgerr.Newf(pgerr.DatatypeMismatch,
			"argument of %s must be an integer, not %s", clause, bound.Type()).At(e.Pos())
	}
}

// at attaches a source position to an error raised by a package that has no
// access to one, such as catalog lookup or type resolution.
func at(err error, pos token.Pos) error {
	if e, ok := errors.AsType[*pgerr.Error](err); ok {
		return e.At(pos)
	}
	return err
}
