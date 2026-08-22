package catalog

import (
	"slices"
	"strings"

	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/types"
)

// The system catalogs, exposed as read-only views computed from the structs in
// this package rather than as real tables that DDL inserts rows into.
//
// The distinction matters more than it looks. A catalog kept as ordinary
// relations makes every CREATE TABLE take data locks on a shared table and turns
// a schema change into a query that can fail halfway, leaving the catalog
// describing a database that does not exist. Computing the rows on read cannot
// drift from the thing it describes, because there is only one copy.
//
// The set of views is the subset tools actually read: enough for a migration
// tool to ask whether a table exists and for a code generator to ask what shape
// it is. It is not a complete information_schema, and the compatibility matrix
// says so rather than leaving it to be discovered.

// InformationSchema and PGCatalog are the two schemas whose contents are
// computed rather than stored.
const (
	InformationSchema = "information_schema"
	PGCatalog         = "pg_catalog"
)

// catalogName is what a single-database engine reports for every *_catalog
// column. PostgreSQL puts the database name there; lightsql has one database
// per instance and no name for it, so this stands in.
const catalogName = "lightsql"

// SystemView is a relation whose rows are derived from the catalog when they are
// read.
type SystemView struct {
	Schema, Name string
	Columns      []Column
}

// QualifiedName returns the name the view is written as.
func (v *SystemView) QualifiedName() string { return v.Schema + "." + v.Name }

// Rows returns the view's current contents.
//
// The builders live in a separate table rather than in a field here, because
// two of these views describe all of the others -- including themselves. A
// single table that both listed the views and held the functions that read that
// list would be an initialisation cycle, which Go refuses outright rather than
// leaving to be discovered at run time.
//
// It is called per execution rather than per bind, so a prepared statement
// re-reads the catalog and sees tables created since it was prepared.
func (v *SystemView) Rows(c *Catalog) [][]types.Value {
	build, ok := rowBuilders[v.QualifiedName()]
	if !ok {
		return nil
	}
	return build(c)
}

// LookupSystemView finds a view by name.
//
// An empty schema searches pg_catalog, and only pg_catalog. That mirrors
// PostgreSQL's implicit search_path, where pg_catalog is searched before
// anything the user wrote -- which is why `SELECT * FROM pg_tables` works
// unqualified while information_schema always has to be named. Tools rely on
// exactly this: goose asks whether its version table exists with an unqualified
// `FROM pg_tables`.
func (c *Catalog) LookupSystemView(schema, name string) (*SystemView, bool) {
	if schema == "" {
		schema = PGCatalog
	}
	if schema != PGCatalog && schema != InformationSchema {
		return nil, false
	}
	for i := range systemViews {
		v := &systemViews[i]
		if v.Schema == schema && v.Name == name {
			return v, true
		}
	}
	return nil, false
}

// SystemViews returns every view, in a stable order.
func SystemViews() []*SystemView {
	out := make([]*SystemView, len(systemViews))
	for i := range systemViews {
		out[i] = &systemViews[i]
	}
	return out
}

// IsSystemSchema reports whether a schema's contents are computed. DDL against
// one is refused: there is nothing to change.
func IsSystemSchema(schema string) bool {
	return schema == PGCatalog || schema == InformationSchema
}

// textCol and intCol build a view's column list.
//
// Every text column is `text` rather than the domain types PostgreSQL uses --
// sql_identifier, character_data, yes_or_no. A domain is a text type with a
// constraint, and nothing here would enforce the constraint, so declaring one
// would claim a check that does not happen. Comparing against a text literal,
// which is what every real query does, behaves identically either way.
func textCol(name string) Column {
	return Column{Name: name, Type: Type{Kind: types.KindText, Name: "text"}}
}

func intCol(name string) Column {
	return Column{Name: name, Type: Type{Kind: types.KindInt, Name: "integer"}}
}

func boolCol(name string) Column {
	return Column{Name: name, Type: Type{Kind: types.KindBool, Name: "boolean"}}
}

// yesNo renders a boolean the way information_schema does. It is text there,
// not a boolean, and a query written against PostgreSQL compares it to the
// string.
func yesNo(b bool) types.Value {
	if b {
		return types.Text("YES")
	}
	return types.Text("NO")
}

// nullIf returns NULL when the condition holds, so a column that does not apply
// reads as absent rather than as a zero that means something.
func nullIf(cond bool, v types.Value) types.Value {
	if cond {
		return types.Null()
	}
	return v
}

var systemViews = []SystemView{
	{
		Schema: InformationSchema,
		Name:   "tables",
		Columns: []Column{
			textCol("table_catalog"), textCol("table_schema"), textCol("table_name"),
			textCol("table_type"), textCol("self_referencing_column_name"),
			textCol("reference_generation"), textCol("user_defined_type_catalog"),
			textCol("user_defined_type_schema"), textCol("user_defined_type_name"),
			textCol("is_insertable_into"), textCol("is_typed"), textCol("commit_action"),
		},
	},
	{
		Schema: InformationSchema,
		Name:   "columns",
		Columns: []Column{
			textCol("table_catalog"), textCol("table_schema"), textCol("table_name"),
			textCol("column_name"), intCol("ordinal_position"), textCol("column_default"),
			textCol("is_nullable"), textCol("data_type"),
			intCol("character_maximum_length"), intCol("numeric_precision"),
			intCol("numeric_scale"), intCol("datetime_precision"),
			textCol("udt_catalog"), textCol("udt_schema"), textCol("udt_name"),
			textCol("is_identity"), textCol("identity_generation"), textCol("is_generated"),
			textCol("is_updatable"),
		},
	},
	{
		Schema: InformationSchema,
		Name:   "table_constraints",
		Columns: []Column{
			textCol("constraint_catalog"), textCol("constraint_schema"),
			textCol("constraint_name"), textCol("table_catalog"),
			textCol("table_schema"), textCol("table_name"), textCol("constraint_type"),
			textCol("is_deferrable"), textCol("initially_deferred"),
		},
	},
	{
		Schema: InformationSchema,
		Name:   "key_column_usage",
		Columns: []Column{
			textCol("constraint_catalog"), textCol("constraint_schema"),
			textCol("constraint_name"), textCol("table_catalog"),
			textCol("table_schema"), textCol("table_name"), textCol("column_name"),
			intCol("ordinal_position"), intCol("position_in_unique_constraint"),
		},
	},
	{
		Schema: PGCatalog,
		Name:   "pg_tables",
		Columns: []Column{
			textCol("schemaname"), textCol("tablename"), textCol("tableowner"),
			textCol("tablespace"), boolCol("hasindexes"), boolCol("hasrules"),
			boolCol("hastriggers"), boolCol("rowsecurity"),
		},
	},
	{
		Schema: PGCatalog,
		Name:   "pg_namespace",
		Columns: []Column{
			intCol("oid"), textCol("nspname"), intCol("nspowner"), textCol("nspacl"),
		},
	},
	{
		Schema: PGCatalog,
		Name:   "pg_class",
		Columns: []Column{
			intCol("oid"), textCol("relname"), intCol("relnamespace"),
			textCol("relkind"), intCol("relnatts"), boolCol("relhasindex"),
			boolCol("relispartition"),
		},
	},
	{
		Schema: PGCatalog,
		Name:   "pg_attribute",
		Columns: []Column{
			intCol("attrelid"), textCol("attname"), intCol("attnum"),
			boolCol("attnotnull"), boolCol("attisdropped"), textCol("atttypname"),
		},
	},
}

// columnRow renders one column of a user table for information_schema.columns.
func columnRow(t *Table, i int) []types.Value {
	col := &t.Columns[i]
	def := types.Null()
	if col.Default != nil {
		if text := defaultText(col.Default); text != "" {
			def = types.Text(text)
		}
	}
	identity := ""
	switch col.Identity {
	case IdentityAlways:
		identity = "ALWAYS"
	case IdentityByDefault:
		identity = "BY DEFAULT"
	}
	return []types.Value{
		types.Text(catalogName), types.Text(t.Schema), types.Text(t.Name),
		types.Text(col.Name), types.Int(int64(i + 1)), def,
		yesNo(!col.NotNull), types.Text(col.Type.Name),
		charMaxLength(col.Type), numericPrecision(col.Type), numericScale(col.Type),
		datetimePrecision(col.Type),
		types.Text(catalogName), types.Text(PGCatalog), types.Text(col.Type.Name),
		yesNo(col.Identity != NotIdentity),
		nullIf(identity == "", types.Text(identity)),
		types.Text("NEVER"), yesNo(true),
	}
}

// viewColumnRow renders one column of a system view, which has no defaults, no
// identity and no length limits.
func viewColumnRow(v *SystemView, i int) []types.Value {
	col := &v.Columns[i]
	return []types.Value{
		types.Text(catalogName), types.Text(v.Schema), types.Text(v.Name),
		types.Text(col.Name), types.Int(int64(i + 1)), types.Null(),
		yesNo(true), types.Text(col.Type.Name),
		types.Null(), numericPrecision(col.Type), numericScale(col.Type), types.Null(),
		types.Text(catalogName), types.Text(PGCatalog), types.Text(col.Type.Name),
		yesNo(false), types.Null(), types.Text("NEVER"), yesNo(false),
	}
}

func constraintRow(t *Table, name, kind string) []types.Value {
	return []types.Value{
		types.Text(catalogName), types.Text(t.Schema), types.Text(name),
		types.Text(catalogName), types.Text(t.Schema), types.Text(t.Name),
		types.Text(kind), yesNo(false), yesNo(false),
	}
}

func keyUsageRow(t *Table, name string, ordinal, position int, inUnique types.Value) []types.Value {
	return []types.Value{
		types.Text(catalogName), types.Text(t.Schema), types.Text(name),
		types.Text(catalogName), types.Text(t.Schema), types.Text(t.Name),
		types.Text(t.Columns[ordinal].Name), types.Int(int64(position)), inUnique,
	}
}

// charMaxLength reports the declared length of a character type, which is the
// first modifier when one was written.
func charMaxLength(ty Type) types.Value {
	if ty.Kind != types.KindText || len(ty.Mods) == 0 {
		return types.Null()
	}
	return types.Int(int64(ty.Mods[0]))
}

// numericPrecision reports the precision of a numeric type. The integer widths
// have a fixed one, and a declared numeric(p, s) carries its own.
func numericPrecision(ty Type) types.Value {
	switch {
	case ty.Kind == types.KindFloat && len(ty.Mods) > 0:
		return types.Int(int64(ty.Mods[0]))
	case ty.Name == "smallint":
		return types.Int(16)
	case ty.Name == "integer":
		return types.Int(32)
	case ty.Name == "bigint":
		return types.Int(64)
	case ty.Name == "real":
		return types.Int(24)
	case ty.Name == "double precision":
		return types.Int(53)
	default:
		return types.Null()
	}
}

func numericScale(ty Type) types.Value {
	if ty.Kind == types.KindFloat && len(ty.Mods) > 1 {
		return types.Int(int64(ty.Mods[1]))
	}
	if ty.Kind == types.KindInt {
		return types.Int(0)
	}
	return types.Null()
}

// datetimePrecision is the number of fractional digits a temporal type keeps.
// lightsql stores microseconds and does not round, so it is always six.
func datetimePrecision(ty Type) types.Value {
	switch ty.Kind {
	case types.KindTime, types.KindTimestamp, types.KindTimestamptz:
		return types.Int(6)
	case types.KindDate:
		return types.Int(0)
	default:
		return types.Null()
	}
}

// schemaNames returns every schema that holds something, user schemas first and
// then the computed ones, so an oid derived from the position is stable for as
// long as the set is.
func (c *Catalog) schemaNames() []string {
	c.mu.RLock()
	seen := make(map[string]bool, len(c.tables))
	for _, t := range c.tables {
		seen[t.Schema] = true
	}
	c.mu.RUnlock()

	seen[DefaultSchema] = true
	names := make([]string, 0, len(seen)+2)
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)
	return append(names, InformationSchema, PGCatalog)
}

// defaultText renders a stored DEFAULT as SQL, which is what
// information_schema.columns reports -- PostgreSQL shows the expression there,
// not the value it would produce.
//
// It is a renderer rather than the original source text because the catalog
// keeps a parsed expression; carrying the text alongside it would be a second
// copy of the same fact, free to disagree with the first. The spelling may
// therefore differ from what was written -- whitespace, a dropped pair of
// redundant parentheses -- while the meaning does not.
//
// An expression this does not know how to render returns the empty string,
// which the caller reports as NULL. Saying nothing is better than saying
// something that does not parse.
func defaultText(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Literal:
		switch e.Kind {
		case ast.LitString:
			// The scanner resolved the escapes, so the quote is put back the
			// way SQL writes it: doubled, not backslashed.
			return "'" + strings.ReplaceAll(e.Val, "'", "''") + "'"
		case ast.LitNumber:
			return e.Val
		case ast.LitTrue:
			return "true"
		case ast.LitFalse:
			return "false"
		default:
			return "NULL"
		}

	case *ast.CurrentTimeExpr:
		return strings.ToUpper(e.Which.String())

	case *ast.FuncCall:
		args := make([]string, len(e.Args))
		for i, a := range e.Args {
			if args[i] = defaultText(a); args[i] == "" {
				return ""
			}
		}
		return e.Name.Name + "(" + strings.Join(args, ", ") + ")"

	case *ast.UnaryExpr:
		x := defaultText(e.X)
		if x == "" {
			return ""
		}
		if e.Op == ast.OpNot {
			return "NOT " + x
		}
		return e.Op.String() + x

	case *ast.ParenExpr:
		x := defaultText(e.X)
		if x == "" {
			return ""
		}
		return "(" + x + ")"

	case *ast.CastExpr:
		x := defaultText(e.X)
		if x == "" {
			return ""
		}
		return "CAST(" + x + " AS " + e.Type.Name + ")"

	default:
		return ""
	}
}

// rowBuilders produces each view's contents; see SystemView.Rows for why they
// are not fields on the view.
var rowBuilders = map[string]func(*Catalog) [][]types.Value{
	"information_schema.tables": func(c *Catalog) [][]types.Value {
		var out [][]types.Value
		for _, t := range c.Tables() {
			out = append(out, []types.Value{
				types.Text(catalogName), types.Text(t.Schema), types.Text(t.Name),
				types.Text("BASE TABLE"), types.Null(), types.Null(),
				types.Null(), types.Null(), types.Null(),
				yesNo(true), yesNo(false), types.Null(),
			})
		}
		// The views list themselves, as they do in PostgreSQL. A query that
		// wants only user tables filters on table_schema, which is what one
		// written against PostgreSQL already does.
		for _, v := range SystemViews() {
			out = append(out, []types.Value{
				types.Text(catalogName), types.Text(v.Schema), types.Text(v.Name),
				types.Text("VIEW"), types.Null(), types.Null(),
				types.Null(), types.Null(), types.Null(),
				yesNo(false), yesNo(false), types.Null(),
			})
		}
		return out
	},
	"information_schema.columns": func(c *Catalog) [][]types.Value {
		var out [][]types.Value
		for _, t := range c.Tables() {
			for i := range t.Columns {
				out = append(out, columnRow(t, i))
			}
		}
		for _, v := range SystemViews() {
			for i := range v.Columns {
				out = append(out, viewColumnRow(v, i))
			}
		}
		return out
	},
	"information_schema.table_constraints": func(c *Catalog) [][]types.Value {
		var out [][]types.Value
		for _, t := range c.Tables() {
			for _, con := range t.Constraints {
				kind := "UNIQUE"
				if con.Kind == PrimaryKeyConstraint {
					kind = "PRIMARY KEY"
				}
				out = append(out, constraintRow(t, con.Name, kind))
			}
			for i := range t.ForeignKeys {
				out = append(out, constraintRow(t, t.ForeignKeys[i].Name, "FOREIGN KEY"))
			}
			for _, ch := range t.Checks {
				out = append(out, constraintRow(t, ch.Name, "CHECK"))
			}
		}
		return out
	},
	"information_schema.key_column_usage": func(c *Catalog) [][]types.Value {
		var out [][]types.Value
		for _, t := range c.Tables() {
			for _, con := range t.Constraints {
				for pos, ord := range con.Columns {
					out = append(out, keyUsageRow(t, con.Name, ord, pos+1, types.Null()))
				}
			}
			for i := range t.ForeignKeys {
				fk := &t.ForeignKeys[i]
				for pos, ord := range fk.Columns {
					out = append(out, keyUsageRow(t, fk.Name, ord, pos+1,
						types.Int(int64(pos+1))))
				}
			}
		}
		return out
	},
	"pg_catalog.pg_tables": func(c *Catalog) [][]types.Value {
		var out [][]types.Value
		for _, t := range c.Tables() {
			out = append(out, []types.Value{
				types.Text(t.Schema), types.Text(t.Name), types.Text(catalogName),
				types.Null(), types.Bool(len(t.Indexes) > 0), types.Bool(false),
				types.Bool(false), types.Bool(false),
			})
		}
		return out
	},
	"pg_catalog.pg_namespace": func(c *Catalog) [][]types.Value {
		var out [][]types.Value
		for i, name := range c.schemaNames() {
			out = append(out, []types.Value{
				types.Int(int64(i + 1)), types.Text(name), types.Int(0), types.Null(),
			})
		}
		return out
	},
	"pg_catalog.pg_class": func(c *Catalog) [][]types.Value {
		schemas := c.schemaNames()
		var out [][]types.Value
		for i, t := range c.Tables() {
			out = append(out, []types.Value{
				types.Int(int64(i + 1)), types.Text(t.Name),
				types.Int(int64(slices.Index(schemas, t.Schema) + 1)),
				types.Text("r"), types.Int(int64(len(t.Columns))),
				types.Bool(len(t.Indexes) > 0), types.Bool(false),
			})
		}
		return out
	},
	"pg_catalog.pg_attribute": func(c *Catalog) [][]types.Value {
		var out [][]types.Value
		for i, t := range c.Tables() {
			for j := range t.Columns {
				col := &t.Columns[j]
				out = append(out, []types.Value{
					types.Int(int64(i + 1)), types.Text(col.Name),
					// attnum is 1-based, as every ordinal PostgreSQL
					// exposes is; the ordinals inside lightsql are not.
					types.Int(int64(j + 1)), types.Bool(col.NotNull),
					types.Bool(false), types.Text(col.Type.Name),
				})
			}
		}
		return out
	},
}
