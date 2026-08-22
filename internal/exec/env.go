package exec

import (
	"github.com/oxisto/lightsql/internal/storage"
	"github.com/oxisto/lightsql/internal/types"
)

// env is what compiling an expression or building an operator needs to know
// beyond the plan itself.
//
// It replaced a bare *storage.Tx parameter when correlated subqueries arrived.
// Those need a second thing threaded to the same places -- the row of the
// enclosing query -- and a struct says once what two parameters would have said
// at every call.
//
// It is created per execution, never stored on a plan node. A prepared
// statement is planned once and executed many times, so a cell hanging off a
// node would be shared by every concurrent execution of it.
type env struct {
	tx *storage.Tx
	// outer holds one cell per enclosing query, innermost last. A correlated
	// subquery writes the row it is being evaluated against into its cell
	// before running, and the expressions compiled inside it read from there.
	//
	// The stack is compile-time structure: which cell an OuterRef reads is
	// fixed when it is compiled, so at run time it costs a pointer
	// dereference and no search.
	outer []*Row
}

// push returns an env one level deeper, with a fresh cell for the subquery
// about to be compiled.
func (e env) push(cell *Row) env {
	// Copied rather than appended in place: two sibling subqueries compiled
	// from the same env must not share a backing array, or the second would
	// overwrite the first's cell.
	next := make([]*Row, len(e.outer), len(e.outer)+1)
	copy(next, e.outer)
	return env{tx: e.tx, outer: append(next, cell)}
}

// outerValue reads a correlated reference: level 0 is the immediately
// enclosing query, 1 the one outside that, and so on.
func (e env) cell(level int) *Row {
	i := len(e.outer) - 1 - level
	if i < 0 || i >= len(e.outer) {
		return nil
	}
	return e.outer[i]
}

// unused keeps the types import honest while the file is small.
var _ = types.Null
