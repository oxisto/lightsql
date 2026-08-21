package binder

import (
	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/catalog"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/plan"
)

// bindCreateIndex resolves CREATE INDEX.
//
// The columns become ordinals here, as everywhere else, so enforcement never
// looks a name up again. A partial index keeps its predicate as syntax, the way
// a CHECK constraint does, and it is bound once per uniqueness check rather
// than stored bound -- the catalog sits below the binder and cannot hold a plan
// expression.
func (b *Binder) bindCreateIndex(s *ast.CreateIndexStmt) (plan.Stmt, error) {
	t, err := b.cat.Lookup(s.Table.Schema.Name, s.Table.Name.Name)
	if err != nil {
		return nil, at(err, s.Table.Pos())
	}

	ix := catalog.Index{Name: s.Name.Name, Unique: s.Unique, Where: s.Where}
	seen := make(map[int]bool, len(s.Columns))
	for _, c := range s.Columns {
		i := t.ColumnIndex(c.Name)
		if i < 0 {
			return nil, pgerr.Newf(pgerr.UndefinedColumn,
				"column %q does not exist", c.Name).At(c.Pos())
		}
		if seen[i] {
			return nil, pgerr.Newf(pgerr.DuplicateColumn,
				"column %q specified more than once", c.Name).At(c.Pos())
		}
		seen[i] = true
		ix.Columns = append(ix.Columns, i)
	}

	// The predicate is bound now purely to reject a bad one at CREATE INDEX
	// rather than at the first insert that would have been checked against it.
	// The catalog keeps the syntax; see catalog.Index.
	if s.Where != nil {
		if _, err := b.BindIndexPredicate(s.Where, t); err != nil {
			return nil, err
		}
	}
	return &plan.CreateIndex{Table: t, Index: ix, IfNotExists: s.IfNotExists}, nil
}

// BindIndexPredicate binds a partial index predicate against its table.
//
// It stops at a bound expression rather than a runnable one, because the binder
// sits below the executor and compiling here would invert that. The engine
// closes over this and exec.Compile to give the catalog something it can run;
// see catalog.SetPredicateCompiler.
func (b *Binder) BindIndexPredicate(expr ast.Expr, t *catalog.Table) (plan.Expr, error) {
	sc := &scope{}
	sc.addTable(t, t.Name)
	return b.bindPredicate(expr, sc, "index predicate")
}

func bindDropIndex(s *ast.DropIndexStmt) (plan.Stmt, error) {
	out := &plan.DropIndex{IfExists: s.IfExists}
	for _, n := range s.Names {
		out.Names = append(out.Names, n.Name)
	}
	return out, nil
}
