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
}

type scopeColumn struct {
	// table is the name the column may be qualified with: the table's alias if
	// it has one, otherwise its name.
	table   string
	name    string
	ordinal int
	typ     catalog.Type
}

// resolve finds a column by an optionally qualified name. An unqualified name
// that matches more than one table is ambiguous, which SQL rejects rather than
// silently picking one.
func (s *scope) resolve(ref *ast.ColumnRef) (scopeColumn, error) {
	var found scopeColumn
	n := 0
	for _, c := range s.cols {
		if c.name != ref.Column.Name {
			continue
		}
		if !ref.Table.IsEmpty() && c.table != ref.Table.Name {
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
	case *ast.InsertStmt:
		return b.bindInsert(s)
	case *ast.SelectStmt:
		return b.bindSelect(s)
	default:
		return nil, pgerr.Newf(pgerr.FeatureNotSupported,
			"statement type %T is not supported yet", stmt).At(stmt.Pos())
	}
}

// ---------------------------------------------------------------------------
// CREATE TABLE
// ---------------------------------------------------------------------------

func (b *Binder) bindCreateTable(s *ast.CreateTableStmt) (plan.Stmt, error) {
	t := &catalog.Table{
		Schema: s.Table.Schema.Name,
		Name:   s.Table.Name.Name,
	}

	for _, cd := range s.Columns {
		typ, err := catalog.ResolveType(cd.Type.Name, cd.Type.Mods)
		if err != nil {
			// The parser knows where the type was written; the resolver does not.
			return nil, at(err, cd.Type.Pos())
		}
		col := catalog.Column{Name: cd.Name.Name, Type: typ}

		for _, c := range cd.Constraints {
			switch c.Kind {
			case ast.ConstraintNotNull:
				col.NotNull = true
			case ast.ConstraintPrimaryKey:
				// PRIMARY KEY implies NOT NULL, as in PostgreSQL.
				col.PrimaryKey = true
				col.NotNull = true
			case ast.ConstraintUnique:
				col.Unique = true
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
		if tc.Kind != ast.ConstraintPrimaryKey {
			return nil, pgerr.Newf(pgerr.FeatureNotSupported,
				"table-level %s constraints are not supported yet", tc.Kind).At(tc.Pos())
		}
		for _, name := range tc.Columns {
			i := indexOfColumn(t.Columns, name.Name)
			if i < 0 {
				return nil, pgerr.Newf(pgerr.UndefinedColumn,
					"column %q named in key does not exist", name.Name).At(name.Pos())
			}
			t.Columns[i].PrimaryKey = true
			t.Columns[i].NotNull = true
		}
	}

	return &plan.CreateTable{Table: t, IfNotExists: s.IfNotExists}, nil
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
	if s.Select != nil {
		return nil, pgerr.New(pgerr.FeatureNotSupported,
			"INSERT ... SELECT is not supported yet").At(s.Pos())
	}
	if len(s.Returning) > 0 {
		return nil, pgerr.New(pgerr.FeatureNotSupported,
			"RETURNING is not supported yet").At(s.Pos())
	}

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
			be, err := bindExpr(e, &scope{})
			if err != nil {
				return nil, err
			}
			if bound[i], err = coerce(be, t.Columns[targets[i]].Type.Kind, e.Pos()); err != nil {
				return nil, err
			}
		}
		ins.Rows = append(ins.Rows, bound)
	}

	// Serial columns the statement did not name are filled from their sequence.
	for i, col := range t.Columns {
		if col.Type.Serial && !slices.Contains(targets, i) {
			ins.Serials = append(ins.Serials, i)
		}
	}
	return ins, nil
}

// ---------------------------------------------------------------------------
// SELECT
// ---------------------------------------------------------------------------

func (b *Binder) bindSelect(s *ast.SelectStmt) (plan.Stmt, error) {
	for _, unsupported := range []struct {
		cond bool
		what string
	}{
		{s.Distinct, "DISTINCT"},
		{len(s.GroupBy) > 0, "GROUP BY"},
		{s.Having != nil, "HAVING"},
		{len(s.OrderBy) > 0, "ORDER BY"},
		{len(s.From) > 1, "multiple FROM items"},
	} {
		if unsupported.cond {
			return nil, pgerr.Newf(pgerr.FeatureNotSupported,
				"%s is not supported yet", unsupported.what).At(s.Pos())
		}
	}

	sc := &scope{}
	var node plan.Node

	if len(s.From) == 1 {
		ref, ok := s.From[0].(*ast.TableRef)
		if !ok {
			return nil, pgerr.New(pgerr.FeatureNotSupported,
				"only a simple table reference is supported in FROM yet").At(s.From[0].Pos())
		}
		t, err := b.cat.Lookup(ref.Table.Schema.Name, ref.Table.Name.Name)
		if err != nil {
			return nil, at(err, ref.Pos())
		}

		// A table is referred to by its alias if it has one, and only by that
		// alias: PostgreSQL hides the original name once aliased.
		qualifier := t.Name
		if !ref.Alias.IsEmpty() {
			qualifier = ref.Alias.Name
		}

		cols := make([]plan.ResultColumn, len(t.Columns))
		for i, c := range t.Columns {
			cols[i] = plan.ResultColumn{Name: c.Name, Type: c.Type}
			sc.cols = append(sc.cols, scopeColumn{
				table: qualifier, name: c.Name, ordinal: i, typ: c.Type,
			})
		}
		node = &plan.Scan{Table: t, Cols: cols}
	}

	if s.Where != nil {
		pred, err := bindExpr(s.Where, sc)
		if err != nil {
			return nil, err
		}
		if pred.Type() != types.KindBool {
			return nil, pgerr.Newf(pgerr.DatatypeMismatch,
				"argument of WHERE must be boolean, not %s", pred.Type()).At(s.Where.Pos())
		}
		if node == nil {
			return nil, pgerr.New(pgerr.SyntaxError, "WHERE requires a FROM clause").At(s.Where.Pos())
		}
		node = &plan.Filter{Input: node, Pred: pred}
	}

	proj, err := b.bindSelectItems(s.Items, sc, node)
	if err != nil {
		return nil, err
	}

	var out plan.Node = proj
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
	return &plan.Query{Root: out}, nil
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
		for _, sc := range sc.cols {
			if sc.ordinal == c.Ordinal {
				return sc.typ
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
	var e *pgerr.Error
	if errors.As(err, &e) {
		return e.At(pos)
	}
	return err
}
