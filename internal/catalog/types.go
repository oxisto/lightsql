package catalog

import (
	"maps"
	"slices"

	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/types"
)

// Type is a resolved column type: the runtime kind the engine stores, plus the
// SQL spelling it was declared with.
//
// Both halves are needed. The kind drives comparison, storage and arithmetic,
// while the declared name is what ColumnType.DatabaseTypeName reports, and ORMs
// introspect that when deciding how to scan a column.
type Type struct {
	Kind types.Kind
	// Name is the canonical SQL name, e.g. "character varying" for VARCHAR.
	Name string
	// Mods are the declared modifiers, e.g. the 255 of varchar(255). They are
	// recorded for introspection; enforcement of length limits is not yet done.
	Mods []int
	// Serial records that the column was declared SERIAL or BIGSERIAL, which in
	// PostgreSQL is shorthand for an integer column defaulting to a sequence.
	Serial bool
}

// typeAliases maps every accepted spelling of a type to its canonical name and
// runtime kind. A table beats a chain of string comparisons scattered through
// the binder, and it makes the set of supported types enumerable — which is what
// lets the compatibility matrix be checked rather than asserted.
var typeAliases = map[string]struct {
	kind      types.Kind
	canonical string
	serial    bool
}{
	// Integers. All widths are stored as int64; the declared width is kept for
	// introspection only.
	"smallint": {types.KindInt, "smallint", false},
	"int2":     {types.KindInt, "smallint", false},
	"int":      {types.KindInt, "integer", false},
	"integer":  {types.KindInt, "integer", false},
	"int4":     {types.KindInt, "integer", false},
	"bigint":   {types.KindInt, "bigint", false},
	"int8":     {types.KindInt, "bigint", false},

	// Serial types are integers with an implicit sequence default.
	"smallserial": {types.KindInt, "smallint", true},
	"serial":      {types.KindInt, "integer", true},
	"serial4":     {types.KindInt, "integer", true},
	"bigserial":   {types.KindInt, "bigint", true},
	"serial8":     {types.KindInt, "bigint", true},

	// Floating point.
	"real":             {types.KindFloat, "real", false},
	"float4":           {types.KindFloat, "real", false},
	"double precision": {types.KindFloat, "double precision", false},
	"float8":           {types.KindFloat, "double precision", false},
	"float":            {types.KindFloat, "double precision", false},

	// Exact numerics currently share the float representation. This is the one
	// place lightsql knowingly diverges from PostgreSQL, so it is called out in
	// the compatibility matrix rather than left to be discovered.
	"numeric": {types.KindFloat, "numeric", false},
	"decimal": {types.KindFloat, "numeric", false},

	// Character types.
	"text":              {types.KindText, "text", false},
	"varchar":           {types.KindText, "character varying", false},
	"character varying": {types.KindText, "character varying", false},
	"char":              {types.KindText, "character", false},
	"character":         {types.KindText, "character", false},
	"bpchar":            {types.KindText, "character", false},
	"uuid":              {types.KindText, "uuid", false},

	"boolean": {types.KindBool, "boolean", false},
	"bool":    {types.KindBool, "boolean", false},

	"bytea": {types.KindBytea, "bytea", false},

	"date":        {types.KindDate, "date", false},
	"time":        {types.KindTime, "time without time zone", false},
	"timestamp":   {types.KindTimestamp, "timestamp without time zone", false},
	"timestamptz": {types.KindTimestamptz, "timestamp with time zone", false},
}

// ResolveType maps a declared type name to the type the engine will store.
// name must already be folded to lower case, as the scanner does.
func ResolveType(name string, mods []int) (Type, error) {
	alias, ok := typeAliases[name]
	if !ok {
		return Type{}, pgerr.Newf(pgerr.UndefinedObject, "type %q does not exist", name)
	}
	return Type{
		Kind:   alias.kind,
		Name:   alias.canonical,
		Mods:   mods,
		Serial: alias.serial,
	}, nil
}

// TypeNames returns every accepted type spelling, for diagnostics and tests.
func TypeNames() []string {
	return slices.Sorted(maps.Keys(typeAliases))
}
