package ast

import (
	"fmt"
	"strings"
)

// Sprint renders a node as an indented S-expression.
//
// This exists so that parser tests can assert on the tree as a whole rather than
// poking at individual fields. A golden file that shows the entire shape of a
// parse makes precedence and associativity mistakes obvious — the tree either
// nests the way the SQL standard says or it visibly does not.
//
// Positions are deliberately omitted: they are checked separately, and including
// them would make every golden file churn on unrelated whitespace edits.
func Sprint(n Node) string {
	var p printer
	p.node(n)
	return p.b.String()
}

type printer struct {
	b      strings.Builder
	indent int
}

// open starts a new list on its own line, indented one level deeper.
func (p *printer) open(head string) {
	if p.b.Len() > 0 {
		p.b.WriteByte('\n')
		p.b.WriteString(strings.Repeat("  ", p.indent))
	}
	p.b.WriteByte('(')
	p.b.WriteString(head)
	p.indent++
}

func (p *printer) close() {
	p.indent--
	p.b.WriteByte(')')
}

// atom appends a leaf on the current line.
func (p *printer) atom(format string, args ...any) {
	p.b.WriteByte(' ')
	fmt.Fprintf(&p.b, format, args...)
}

// field prints a named sub-list, skipping it entirely when the clause is absent
// so that golden files stay readable.
func (p *printer) field(name string, n Node) {
	if n == nil {
		return
	}
	p.open(name)
	p.node(n)
	p.close()
}

func (p *printer) exprList(name string, xs []Expr) {
	if len(xs) == 0 {
		return
	}
	p.open(name)
	for _, x := range xs {
		p.node(x)
	}
	p.close()
}

func (p *printer) name(n Name) {
	if n.IsEmpty() {
		return
	}
	if n.Quoted {
		p.atom("%q", n.Name)
		return
	}
	p.atom("%s", n.Name)
}

func (p *printer) node(n Node) {
	switch n := n.(type) {
	case nil:
		return

	// Statements.
	case *SelectStmt:
		p.open("select")
		if n.Distinct {
			if len(n.DistinctOn) > 0 {
				p.exprList("distinct-on", n.DistinctOn)
			} else {
				p.atom("distinct")
			}
		}
		p.open("items")
		for _, it := range n.Items {
			if it.Alias.IsEmpty() {
				p.node(it.Expr)
				continue
			}
			p.open("as")
			p.name(it.Alias)
			p.node(it.Expr)
			p.close()
		}
		p.close()
		if len(n.From) > 0 {
			p.open("from")
			for _, f := range n.From {
				p.node(f)
			}
			p.close()
		}
		p.field("where", n.Where)
		p.exprList("group-by", n.GroupBy)
		p.field("having", n.Having)
		if len(n.OrderBy) > 0 {
			p.open("order-by")
			for _, o := range n.OrderBy {
				p.open("term")
				p.atom("%s", strings.ToLower(sortDirName(o.Dir)))
				if o.Nulls != NullsDefault {
					p.atom("%s", strings.ToLower(nullsOrderName(o.Nulls)))
				}
				p.node(o.Expr)
				p.close()
			}
			p.close()
		}
		p.field("limit", n.Limit)
		p.field("offset", n.Offset)
		p.close()

	case *InsertStmt:
		p.open("insert")
		p.atom("%s", n.Table)
		if len(n.Columns) > 0 {
			p.open("columns")
			for _, c := range n.Columns {
				p.name(c)
			}
			p.close()
		}
		for _, row := range n.Rows {
			p.exprList("values", row)
		}
		if n.Select != nil {
			p.field("select", n.Select)
		}
		if len(n.Returning) > 0 {
			p.open("returning")
			for _, it := range n.Returning {
				p.node(it.Expr)
			}
			p.close()
		}
		p.close()

	case *CreateTableStmt:
		p.open("create-table")
		p.atom("%s", n.Table)
		if n.IfNotExists {
			p.atom("if-not-exists")
		}
		for _, c := range n.Columns {
			p.node(c)
		}
		for _, c := range n.Constraints {
			p.node(c)
		}
		p.close()

	case *ColumnDef:
		p.open("column")
		p.name(n.Name)
		p.node(n.Type)
		for _, c := range n.Constraints {
			p.node(c)
		}
		p.close()

	case *ColumnConstraint:
		p.open("constraint")
		p.atom("%s", strings.ToLower(n.Kind.String()))
		p.name(n.Name)
		p.node(n.Expr)
		p.printRef(n.Ref)
		p.close()

	case *TableConstraint:
		p.open("table-constraint")
		p.atom("%s", strings.ToLower(n.Kind.String()))
		p.name(n.Name)
		for _, c := range n.Columns {
			p.name(c)
		}
		p.node(n.Expr)
		p.printRef(n.Ref)
		p.close()

	case *TypeName:
		p.open("type")
		p.atom("%s", n.Name)
		for _, m := range n.Mods {
			p.atom("%d", m)
		}
		p.close()

	// FROM clause.
	case *TableRef:
		p.open("table")
		p.atom("%s", n.Table)
		if !n.Alias.IsEmpty() {
			p.atom("as")
			p.name(n.Alias)
		}
		p.close()

	case *SubqueryRef:
		p.open("derived")
		if !n.Alias.IsEmpty() {
			p.atom("as")
			p.name(n.Alias)
		}
		p.node(n.Select)
		p.close()

	case *JoinExpr:
		p.open("join")
		p.atom("%s", strings.ToLower(n.Type.String()))
		p.node(n.Left)
		p.node(n.Right)
		p.field("on", n.On)
		if len(n.Using) > 0 {
			p.open("using")
			for _, u := range n.Using {
				p.name(u)
			}
			p.close()
		}
		p.close()

	// Expressions.
	case *Literal:
		switch n.Kind {
		case LitString:
			p.open("lit")
			p.atom("%q", n.Val)
		case LitNumber:
			p.open("lit")
			p.atom("%s", n.Val)
		case LitTrue:
			p.open("lit")
			p.atom("true")
		case LitFalse:
			p.open("lit")
			p.atom("false")
		default:
			p.open("lit")
			p.atom("null")
		}
		p.close()

	case *Param:
		p.open("param")
		p.atom("%d", n.Ord)
		p.close()

	case *ColumnRef:
		p.open("col")
		p.atom("%s", n)
		p.close()

	case *Star:
		p.open("star")
		p.name(n.Table)
		p.close()

	case *BinaryExpr:
		p.open(opTag(n.Op.String()))
		p.node(n.X)
		p.node(n.Y)
		p.close()

	case *UnaryExpr:
		p.open(unaryOpTag(n.Op))
		p.node(n.X)
		p.close()

	case *ParenExpr:
		// Parentheses carry no meaning once the tree is built; printing them
		// would hide whether precedence was actually applied.
		p.node(n.X)

	case *IsNullExpr:
		if n.Negate {
			p.open("is-not-null")
		} else {
			p.open("is-null")
		}
		p.node(n.X)
		p.close()

	case *CastExpr:
		p.open("cast")
		p.node(n.Type)
		p.node(n.X)
		p.close()

	case *FuncCall:
		p.open("call")
		p.name(n.Name)
		if n.Distinct {
			p.atom("distinct")
		}
		if n.Star {
			p.atom("*")
		}
		for _, a := range n.Args {
			p.node(a)
		}
		p.close()

	case *InExpr:
		if n.Negate {
			p.open("not-in")
		} else {
			p.open("in")
		}
		p.node(n.X)
		if n.Subquery != nil {
			p.node(n.Subquery)
		} else {
			p.exprList("list", n.List)
		}
		p.close()

	case *BetweenExpr:
		if n.Negate {
			p.open("not-between")
		} else {
			p.open("between")
		}
		p.node(n.X)
		p.node(n.Lo)
		p.node(n.Hi)
		p.close()

	case *CaseExpr:
		p.open("case")
		p.field("operand", n.Operand)
		for _, w := range n.Whens {
			p.open("when")
			p.node(w.Cond)
			p.node(w.Value)
			p.close()
		}
		p.field("else", n.Else)
		p.close()

	case *ExistsExpr:
		if n.Negate {
			p.open("not-exists")
		} else {
			p.open("exists")
		}
		p.node(n.Subquery)
		p.close()

	case *SubqueryExpr:
		p.open("subquery")
		p.node(n.Select)
		p.close()

	default:
		p.open("UNKNOWN")
		p.atom("%T", n)
		p.close()
	}
}

func (p *printer) printRef(r *ForeignKeyRef) {
	if r == nil {
		return
	}
	p.open("references")
	p.atom("%s", r.Table)
	for _, c := range r.Columns {
		p.name(c)
	}
	if r.OnDelete != NoAction {
		p.atom("on-delete=%s", strings.ReplaceAll(strings.ToLower(r.OnDelete.String()), " ", "-"))
	}
	if r.OnUpdate != NoAction {
		p.atom("on-update=%s", strings.ReplaceAll(strings.ToLower(r.OnUpdate.String()), " ", "-"))
	}
	p.close()
}

// opTag renders an operator name as a single S-expression head, so that a
// multi-word operator does not look like an operator plus operands.
func opTag(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), " ", "-")
}

// unaryOpTag names a prefix operator readably, since "-" and "+" would be
// indistinguishable from their binary forms in a printed tree.
func unaryOpTag(o UnaryOp) string {
	switch o {
	case OpNeg:
		return "neg"
	case OpPlus:
		return "pos"
	default:
		return "not"
	}
}

func sortDirName(d SortDir) string {
	if d == SortDesc {
		return "DESC"
	}
	return "ASC"
}

func nullsOrderName(n NullsOrder) string {
	if n == NullsFirst {
		return "NULLS-FIRST"
	}
	return "NULLS-LAST"
}
