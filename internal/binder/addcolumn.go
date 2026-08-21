package binder

import (
	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/types"
)

// bindAddColumn resolves ALTER TABLE ... ADD COLUMN.
//
// The rows already stored are not rewritten. They stay at their old width and
// read the new column as its missing value, which is the DEFAULT if one was
// written and NULL otherwise -- so the value is settled here, once, rather than
// re-derived per row.
func (b *Binder) bindAddColumn(s *ast.AlterTableStmt, a *ast.AddColumn) (plan.Stmt, error) {
	t, err := b.cat.Lookup(s.Table.Schema.Name, s.Table.Name.Name)
	if err != nil {
		return nil, at(err, s.Table.Pos())
	}

	cd := a.Column
	typ, err := catalog.ResolveType(cd.Type.Name, cd.Type.Mods)
	if err != nil {
		return nil, at(err, cd.Type.Pos())
	}

	out := &plan.AddColumn{
		Table:       t,
		IfNotExists: a.IfNotExists,
		Column:      catalog.Column{Name: cd.Name.Name, Type: typ},
	}
	ordinal := len(t.Columns)

	for _, c := range cd.Constraints {
		switch c.Kind {
		case ast.ConstraintNotNull:
			out.Column.NotNull = true
		case ast.ConstraintNull:
			// Explicit NULL is the default and carries no information.
		case ast.ConstraintDefault:
			// Evaluated now rather than stored as syntax, because it has to
			// become the value the existing rows read. A DEFAULT is a constant
			// expression, so there is nothing to evaluate it against.
			v, err := b.EvalConstDefault(c.Expr, typ.Kind)
			if err != nil {
				return nil, err
			}
			out.Column.Default = c.Expr
			out.Missing = v
		case ast.ConstraintCheck:
			out.Check = &catalog.Check{Name: c.Name.Name, Expr: c.Expr}
		case ast.ConstraintReferences:
			fk, err := b.resolveForeignKey(t, refSpec{
				name: c.Name.Name, cols: []int{ordinal}, ref: c.Ref, pos: c.Pos(),
				// The column is not in the table yet, so the reference has to
				// be told what it is referencing with.
				pending: &out.Column,
			})
			if err != nil {
				return nil, err
			}
			out.ForeignKey = &fk
		default:
			// PRIMARY KEY and UNIQUE would have to hold over the rows already
			// stored, which all read the same missing value -- so either every
			// existing row collides, or the table is empty and the constraint
			// belongs in CREATE TABLE. Refusing says so rather than failing
			// later with a duplicate-key error that names no row.
			return nil, pgerr.Newf(pgerr.FeatureNotSupported,
				"%s is not supported on ADD COLUMN", c.Kind).At(c.Pos())
		}
	}

	// A NOT NULL column with no default would be violated by every row already
	// stored, since they all read it as NULL. PostgreSQL refuses for the same
	// reason, and refusing here beats writing a table that cannot be read back.
	if out.Column.NotNull && out.Missing.IsNull() {
		return nil, pgerr.Newf(pgerr.NotNullViolation,
			"column %q of relation %q contains null values",
			cd.Name.Name, t.Name).At(cd.Name.Pos())
	}
	if out.Missing.Kind() == types.KindNull {
		out.Missing = types.Null()
	}
	return out, nil
}
