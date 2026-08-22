package exec

import (
	"context"

	"github.com/oxisto/lightsql/internal/plan"
	"github.com/oxisto/lightsql/internal/storage"
	"github.com/oxisto/lightsql/internal/types"
)

// Build turns a plan into the operator tree that produces its rows.
//
// The environment the tree is compiled with is created here rather than passed
// in, because it is per execution: it carries the cells a correlated subquery
// reads its enclosing row from, and a prepared statement executed twice at once
// must not share them.
func Build(ctx context.Context, n plan.Node, tx *storage.Tx, args []types.Value) (Operator, error) {
	return build(ctx, n, env{tx: tx}, args)
}

// Compile turns an expression into a function over a row.
//
// A nil transaction is legal and means the expression is being folded outside
// any statement, which is how a DEFAULT is evaluated to satisfy a referential
// SET DEFAULT.
func Compile(e plan.Expr, tx *storage.Tx) (Eval, error) {
	return compile(e, env{tx: tx})
}
