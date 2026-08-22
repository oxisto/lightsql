// Package features is the single source of truth for lightsql's SQL
// compatibility matrix.
//
// The table in README.md is generated from this registry, and a test regenerates
// and diffs it, so the documented compatibility cannot drift from the code. Each
// entry also carries a probe: a representative statement that must parse when the
// front end claims support and must be rejected when it does not. That turns the
// matrix from a promise into an assertion — a feature cannot be marked supported
// without at least one statement proving it.
//
// Adding a SQL feature therefore includes updating this file; see the
// sql-feature skill in .claude/skills.
package features

import (
	"fmt"
	"strings"
)

// Status describes how far a feature has been implemented.
type Status uint8

const (
	// No means the feature is deliberately out of scope.
	No Status = iota
	// Planned means it is intended but not implemented.
	Planned
	// Partial means the common forms work but some do not; Note says which.
	Partial
	// Yes means fully implemented.
	Yes
)

// mark is the cell rendered in the README for each status.
var mark = map[Status]string{
	Yes:     "✅",
	Partial: "🟡",
	Planned: "⬜",
	No:      "❌",
}

func (s Status) String() string {
	switch s {
	case Yes:
		return "yes"
	case Partial:
		return "partial"
	case Planned:
		return "planned"
	default:
		return "no"
	}
}

// Feature is one row of the compatibility matrix.
//
// Parse and Exec are tracked separately because the front end legitimately runs
// ahead of the executor during development, and collapsing them into one column
// would either overstate or understate what works.
type Feature struct {
	// Name is the SQL construct, written as it appears in the standard.
	Name string
	// Parse is the status of the scanner, parser and AST.
	Parse Status
	// Exec is the status of the binder, planner and executor.
	Exec Status
	// Note explains a Partial status or records a caveat. Keep it to a clause.
	Note string
	// SQL is a representative statement used as a probe. It must be a complete,
	// self-contained statement. Leave empty only when no single statement can
	// demonstrate the feature.
	SQL string
	// Setup holds statements run before SQL in a fresh instance, so that a probe
	// can refer to a table. Without it, an execution probe could only ever
	// demonstrate features that need no schema.
	Setup []string
}

// Overall reduces the two axes to the status a user actually experiences: a
// feature the parser accepts but the executor cannot run is not usable.
func (f Feature) Overall() Status {
	return min(f.Parse, f.Exec)
}

// Group is a named section of the matrix.
type Group struct {
	Name     string
	Features []Feature
}

// probeTable is the fixture most query probes run against. Sharing one schema
// keeps each probe's SQL about the feature rather than the scaffolding.
var probeTable = []string{
	`CREATE TABLE t (a INT, b INT, c INT, s TEXT, flag BOOLEAN)`,
}

// jsonTable carries a row, because a JSON probe that returns no rows would
// never evaluate the operator it is meant to prove works.
var jsonTable = []string{
	`CREATE TABLE j (doc JSONB, raw JSON)`,
	`INSERT INTO j VALUES ('{"a":1}', '{"a":1}')`,
}

// probeJoin adds a second table, for probes that need two.
var probeJoin = []string{
	`CREATE TABLE t (a INT, b INT, id INT, s TEXT)`,
	`CREATE TABLE u (a INT, b INT, id INT)`,
}

// Groups is the registry. Order here is the order in the README.
var Groups = []Group{
	{
		Name: "Data definition",
		Features: []Feature{
			{Name: "CREATE TABLE", Parse: Yes, Exec: Yes,
				SQL: "CREATE TABLE t (id INT, name TEXT)"},
			{Name: "CREATE TABLE IF NOT EXISTS", Parse: Yes, Exec: Yes,
				SQL: "CREATE TABLE IF NOT EXISTS t (id INT)"},
			{Name: "Column constraints", Parse: Yes, Exec: Yes,
				Note:  "NOT NULL, PRIMARY KEY, UNIQUE, DEFAULT, CHECK and REFERENCES are all enforced",
				Setup: []string{"CREATE TABLE t (id INT PRIMARY KEY, e TEXT UNIQUE, n INT NOT NULL, d INT DEFAULT 7, c INT CHECK (c > 0))"},
				SQL:   "INSERT INTO t (id, e, n, c) VALUES (1, 'a', 0, 1)"},
			{Name: "Table constraints", Parse: Yes, Exec: Yes,
				Note:  "PRIMARY KEY, UNIQUE, CHECK and FOREIGN KEY, with keys over several columns requiring the combination to be unique",
				Setup: []string{"CREATE TABLE t (a INT, b INT, c INT, PRIMARY KEY (a, b), UNIQUE (c), CHECK (c > 0))"},
				SQL:   "INSERT INTO t (a, b, c) VALUES (1, 1, 1)"},
			{Name: "DEFAULT values", Parse: Yes, Exec: Yes,
				Note:  "any constant expression; an omitted column takes it, an explicit NULL overrides it",
				Setup: []string{"CREATE TABLE t (a INT DEFAULT 6 * 7, s TEXT DEFAULT 'x')"},
				SQL:   "INSERT INTO t (s) VALUES ('y')"},
			{Name: "CHECK constraints", Parse: Yes, Exec: Yes,
				Note:  "column and table level, enforced on insert and update; satisfied by true or unknown, so a NULL does not violate one",
				Setup: []string{"CREATE TABLE t (n INT CHECK (n >= 0))"},
				SQL:   "INSERT INTO t (n) VALUES (1)"},
			{Name: "Foreign keys", Parse: Yes, Exec: Yes,
				Note:  "column and table level, single and multi-column; a NULL in the key is unconstrained, as MATCH SIMPLE requires",
				Setup: []string{"CREATE TABLE u (id INT PRIMARY KEY)"},
				SQL:   "CREATE TABLE t (a INT REFERENCES u (id))"},
			{Name: "Referential actions", Parse: Yes, Exec: Yes,
				Note: "ON DELETE and ON UPDATE with CASCADE, RESTRICT, NO ACTION, SET NULL " +
					"and SET DEFAULT; cascades are part of the transaction, recurse past the " +
					"immediate children, and terminate on a cyclic reference",
				Setup: []string{"CREATE TABLE u (id INT PRIMARY KEY)"},
				SQL:   "CREATE TABLE t (a INT REFERENCES u (id) ON DELETE CASCADE ON UPDATE SET NULL)"},
			{Name: "DROP TABLE", Parse: Yes, Exec: Yes,
				Note: "several tables at once, IF EXISTS, and RESTRICT which is the default: " +
					"a table another one references is kept, unless both are named in the same " +
					"statement. CASCADE parses but is refused rather than half-performed. " +
					"Like every DDL statement it is not transactional, so a rolled back drop " +
					"does not bring the table back",
				Setup: probeTable,
				SQL:   "DROP TABLE t"},
			{Name: "ALTER TABLE", Parse: Partial, Exec: Partial,
				Note: "ADD COLUMN, ALTER COLUMN SET and DROP NOT NULL, RENAME TO and " +
					"RENAME COLUMN. Adding a column does not " +
					"rewrite the rows already stored: they stay shorter than the table and " +
					"read it as its DEFAULT, or NULL, which is what PostgreSQL calls a " +
					"missing value. NOT NULL without a DEFAULT is refused, since every stored " +
					"row would violate it; SET NOT NULL afterwards is how a backfilled column " +
					"is tightened, and it is checked against the rows already there. A " +
					"foreign key survives a rename, holding a table " +
					"pointer and ordinals rather than names, but renaming a column that a " +
					"CHECK, a DEFAULT or a partial index predicate names is refused, since " +
					"those are stored as syntax. DROP COLUMN and a type change are not " +
					"supported: neither can be served by a missing value. SET and DROP " +
					"DEFAULT are not supported yet",
				Setup: probeTable,
				SQL:   "ALTER TABLE t ADD COLUMN added INT NOT NULL DEFAULT 0; ALTER TABLE t ALTER COLUMN added DROP NOT NULL"},
			{Name: "CREATE INDEX", Parse: Yes, Exec: Partial,
				Note: "UNIQUE and partial indexes are enforced as constraints, including a " +
					"partial one whose predicate decides which rows it covers. A plain index " +
					"is recorded but builds no structure and is never chosen, because there " +
					"is no index selection in the planner yet -- so it costs nothing and " +
					"speeds nothing up. DROP INDEX is supported; expression indexes and a " +
					"per-column sort order are not",
				Setup: probeTable,
				SQL:   "CREATE INDEX i ON t (a)"},
			{Name: "CREATE VIEW", Parse: Planned, Exec: Planned, SQL: "CREATE VIEW v AS SELECT 1"},
			{Name: "CREATE SCHEMA", Parse: Planned, Exec: Planned, SQL: "CREATE SCHEMA s"},
			{Name: "Sequences and SERIAL", Parse: Yes, Exec: Yes,
				Note:  "an omitted SERIAL column is filled from a per-column sequence",
				Setup: []string{"CREATE TABLE s (id BIGSERIAL PRIMARY KEY, v INT)"},
				SQL:   "INSERT INTO s (v) VALUES (1)"},
		},
	},
	{
		Name: "Data manipulation",
		Features: []Feature{
			{Name: "INSERT ... VALUES", Parse: Yes, Exec: Yes,
				Note:  "including multi-row VALUES",
				Setup: probeTable,
				SQL:   "INSERT INTO t (a, s) VALUES (1, 'x'), (2, 'y')"},
			{Name: "INSERT ... SELECT", Parse: Yes, Exec: Yes,
				Note: "the source may be any query, and its rows go through the same " +
					"serial, DEFAULT, CHECK and RETURNING handling as VALUES. Reading the " +
					"table being written is safe, since a scan takes its rows when the " +
					"operator is built",
				Setup: probeJoin,
				SQL:   "INSERT INTO t (a) SELECT b FROM u"},
			{Name: "RETURNING", Parse: Yes, Exec: Yes,
				Note:  "on INSERT, UPDATE and DELETE; sees generated serial values",
				Setup: probeTable,
				SQL:   "INSERT INTO t (a) VALUES (1) RETURNING a"},
			{Name: "UPDATE", Parse: Yes, Exec: Yes,
				Note:  "assignments all read the original row, so SET a = b, b = a swaps",
				Setup: probeTable,
				SQL:   "UPDATE t SET a = a + 1 WHERE b = 2"},
			{Name: "DELETE", Parse: Yes, Exec: Yes,
				Note:  "row order is preserved for the rows that remain",
				Setup: probeTable,
				SQL:   "DELETE FROM t WHERE a = 1"},
			{Name: "ON CONFLICT", Parse: Yes, Exec: Yes,
				Note: "DO NOTHING, with or without a target, and DO UPDATE with an optional " +
					"WHERE. The update sees the stored row by table name and the proposed one " +
					"as excluded. A target must be covered by a primary key, unique constraint " +
					"or total unique index, since one nothing enforces would never detect a " +
					"collision. A skip reports zero rows affected",
				Setup: []string{"CREATE TABLE t (a INT PRIMARY KEY, b INT)"},
				SQL:   "INSERT INTO t (a) VALUES (1) ON CONFLICT (a) DO NOTHING"},
			{Name: "TRUNCATE", Parse: Planned, Exec: Planned, SQL: "TRUNCATE t"},
		},
	},
	{
		Name: "Queries",
		Features: []Feature{
			{Name: "SELECT list, aliases", Parse: Yes, Exec: Yes,
				Note:  "AS is optional",
				Setup: probeTable,
				SQL:   "SELECT a AS x, b y, * FROM t"},
			{Name: "SELECT without FROM", Parse: Yes, Exec: Yes,
				SQL: "SELECT 1 + 1"},
			{Name: "WHERE", Parse: Yes, Exec: Yes,
				Setup: probeTable,
				SQL:   "SELECT a FROM t WHERE a > 1"},
			{Name: "LIMIT / OFFSET", Parse: Yes, Exec: Yes,
				Note:  "either order, LIMIT ALL accepted",
				Setup: probeTable,
				SQL:   "SELECT a FROM t LIMIT 10 OFFSET 5"},
			{Name: "Table aliases", Parse: Yes, Exec: Yes,
				Note:  "an alias replaces the table name, as in PostgreSQL",
				Setup: probeTable,
				SQL:   "SELECT x.a FROM t x WHERE x.b > 1"},
			{Name: "Inner and outer joins", Parse: Yes, Exec: Yes,
				Note:  "INNER, LEFT, RIGHT, FULL and CROSS, with ON or USING; a comma in FROM is a cross join; nested loop, so no index is used yet",
				Setup: probeJoin,
				SQL:   "SELECT t.a FROM t LEFT OUTER JOIN u ON t.id = u.id"},
			{Name: "JOIN ... USING", Parse: Yes, Exec: Yes,
				Note:  "the pair is merged into one column, so it is unambiguous unqualified and appears once in SELECT *",
				Setup: probeJoin,
				SQL:   "SELECT id FROM t JOIN u USING (id)"},
			{Name: "GROUP BY / HAVING", Parse: Yes, Exec: Yes,
				Note:  "groups on columns or expressions; NULLs form one group; HAVING may use an aggregate the select list does not",
				Setup: probeTable,
				SQL:   "SELECT a FROM t GROUP BY a HAVING count(*) > 1"},
			{Name: "Aggregate functions", Parse: Yes, Exec: Partial,
				Note:  "count, sum, avg, min and max, each with DISTINCT; count is 0 over no rows and the rest are NULL. Other aggregates are pending",
				Setup: probeTable,
				SQL:   "SELECT count(*), sum(a), avg(b), min(c), max(a) FROM t"},
			{Name: "ORDER BY", Parse: Yes, Exec: Yes,
				Note:  "ASC/DESC, NULLS FIRST/LAST, output aliases, select-list positions, and expressions over unselected columns",
				Setup: probeTable,
				SQL:   "SELECT a FROM t ORDER BY a DESC NULLS FIRST, b"},
			{Name: "DISTINCT / DISTINCT ON", Parse: Yes, Exec: Yes,
				Note:  "compares the output row, and treats NULLs as equal; DISTINCT ON keeps the first row per key, so ORDER BY decides which. Unlike PostgreSQL, ORDER BY on an unselected column is accepted rather than rejected",
				Setup: probeTable,
				SQL:   "SELECT DISTINCT ON (a) a, b FROM t"},
			{Name: "Subqueries", Parse: Yes, Exec: Partial,
				Note: "scalar, IN, EXISTS and derived tables, which must have an alias. " +
					"A scalar subquery is NULL over no rows and an error over more than one. " +
					"Only uncorrelated subqueries are supported: one that references the " +
					"outer query, and LATERAL, are both rejected rather than mis-resolved",
				Setup: probeJoin,
				SQL:   "SELECT a FROM t WHERE EXISTS (SELECT 1 FROM u)"},
			{Name: "UNION / INTERSECT / EXCEPT", Parse: Planned, Exec: Planned,
				SQL: "SELECT a FROM t UNION SELECT b FROM u"},
			{Name: "Common table expressions", Parse: Planned, Exec: Planned,
				Note: "WITH, including RECURSIVE",
				SQL:  "WITH x AS (SELECT 1) SELECT * FROM x"},
			{Name: "Window functions", Parse: No, Exec: No,
				Note: "out of scope for v1",
				SQL:  "SELECT row_number() OVER (ORDER BY a) FROM t"},
		},
	},
	{
		Name: "Expressions",
		Features: []Feature{
			{Name: "Operator precedence", Parse: Yes, Exec: Yes,
				Note:  "full PostgreSQL precedence table, including left-associative ^",
				Setup: probeTable,
				SQL:   "SELECT a * b + c, -2 ^ 2 FROM t"},
			{Name: "Comparison and logic", Parse: Yes, Exec: Yes,
				Note:  "three-valued logic throughout",
				Setup: probeTable,
				SQL:   "SELECT a FROM t WHERE a = 1 AND NOT b <> 2 OR c >= 3"},
			{Name: "IS NULL / IS DISTINCT FROM", Parse: Yes, Exec: Yes,
				Setup: probeTable,
				SQL:   "SELECT a FROM t WHERE a IS NULL AND b IS NOT DISTINCT FROM c"},
			{Name: "String concatenation", Parse: Yes, Exec: Yes,
				Note:  "NULL propagates, as in PostgreSQL",
				Setup: probeTable,
				SQL:   "SELECT s || 'x' FROM t"},
			{Name: "Parameter placeholders", Parse: Yes, Exec: Yes,
				Note:  "$1 and ?, not mixed in one statement; the type is inferred from context",
				Setup: probeTable,
				SQL:   "SELECT a FROM t WHERE b = $1 AND c = $2"},
			{Name: "BETWEEN / IN / LIKE", Parse: Yes, Exec: Yes,
				Note: "including the negated forms. BETWEEN is inclusive and rewritten to " +
					"a pair of comparisons; LIKE supports % and _ with backslash escaping, " +
					"and is anchored, so it matches the whole string. IN follows SQL's " +
					"three-valued rule: without a match, a NULL among the candidates makes " +
					"the answer unknown rather than false, so NOT IN over a NULL returns " +
					"no rows. ILIKE and an explicit ESCAPE clause are not supported",
				Setup: probeTable,
				SQL:   "SELECT a FROM t WHERE a BETWEEN 1 AND 2 AND s LIKE 'x%' AND a IN (1, 2)"},
			{Name: "CASE", Parse: Yes, Exec: Yes,
				Note: "simple and searched forms; the simple form is rewritten to the " +
					"searched one, so both take one path. Only a true condition fires an " +
					"arm, no match without ELSE is NULL, and the branches must share a " +
					"type so a result column cannot change type from row to row",
				Setup: probeTable,
				SQL:   "SELECT CASE WHEN a > 1 THEN 1 ELSE 2 END FROM t"},
			{Name: "CAST", Parse: Yes, Exec: Yes,
				Note:  "both CAST(x AS t) and x::t",
				Setup: probeTable,
				SQL:   "SELECT CAST(a AS TEXT) FROM t"},
			{Name: "Scalar functions", Parse: Yes, Exec: Partial,
				Note: "coalesce, nullif, now, lower, upper, length, trim, abs and round. " +
					"NULL propagates for all but coalesce and nullif, and coalesce stops at " +
					"the first argument that answers. Argument types are checked at bind " +
					"time, so lower(1) is rejected rather than reading an integer as text. " +
					"The library is small and grows on demand",
				Setup: probeTable,
				SQL:   "SELECT coalesce(a, b, 0) FROM t"},
			{Name: "Arrays", Parse: No, Exec: No, Note: "out of scope for v1"},
			// JSONB is in scope: it is one of the types the target applications
			// actually store, so a test suite that cannot round-trip a JSONB
			// column cannot use lightsql at all.
			{Name: "JSONB", Parse: Yes, Exec: Yes,
				Note:  "canonicalised on store; scans as []byte",
				Setup: jsonTable,
				SQL:   `SELECT doc -> 'a', doc ->> 'a' FROM j WHERE doc @> '{"a":1}'`},
			{Name: "JSON", Parse: Yes, Exec: Yes,
				Note:  "kept exactly as written, unlike jsonb",
				Setup: jsonTable,
				SQL:   `SELECT raw ->> 'a' FROM j`},
		},
	},
	{
		Name: "Types",
		Features: []Feature{
			{Name: "Integer types", Parse: Yes, Exec: Yes,
				Note: "SMALLINT, INT, BIGINT stored as int64",
				SQL:  "CREATE TABLE t (a SMALLINT, b INT, c BIGINT)"},
			{Name: "Floating point", Parse: Yes, Exec: Yes,
				Note: "REAL and DOUBLE PRECISION stored as float64",
				SQL:  "CREATE TABLE t (a REAL, b DOUBLE PRECISION)"},
			{Name: "Character types", Parse: Yes, Exec: Yes,
				Note: "TEXT, VARCHAR(n), CHARACTER VARYING(n), CHAR; length is recorded but not enforced",
				SQL:  "CREATE TABLE t (a TEXT, b VARCHAR(255), c CHARACTER VARYING(10))"},
			{Name: "BOOLEAN", Parse: Yes, Exec: Yes, SQL: "CREATE TABLE t (a BOOLEAN)"},
			{Name: "Date and time", Parse: Yes, Exec: Partial,
				Note: "columns, time.Time arguments and ISO 8601 literals, with either a " +
					"space or a T separator. A zone offset is honoured by timestamptz and " +
					"dropped by timestamp, as \"without time zone\" requires. A bare literal " +
					"takes its type from the column it is compared or assigned to. " +
					"INTERVAL is pending, and the non-ISO date " +
					"styles PostgreSQL accepts are deliberately not, since 01/02/2024 has no " +
					"reading that is right in both conventions",
				Setup: []string{"CREATE TABLE t (a DATE, b TIMESTAMP WITH TIME ZONE, c TIMESTAMP)"},
				SQL:   "INSERT INTO t VALUES ('2024-01-02', '2024-01-02T12:30:00+02:00', '2024-01-02 12:30:00')"},
			// Split from the row above, which used to say now() was pending
			// long after it worked. A note is the one part of this registry the
			// probes cannot check, so it is the part that goes stale.
			{Name: "Current date and time", Parse: Yes, Exec: Partial,
				Note: "now(), CURRENT_TIMESTAMP, LOCALTIMESTAMP, CURRENT_DATE, CURRENT_TIME " +
					"and LOCALTIME, all reporting the transaction start so that one " +
					"transaction cannot disagree with itself. CURRENT_TIME is a plain time " +
					"rather than PostgreSQL's zoned one, and a precision argument such as " +
					"CURRENT_TIMESTAMP(0) is refused rather than ignored",
				SQL: "SELECT now(), CURRENT_TIMESTAMP, LOCALTIMESTAMP, CURRENT_DATE, CURRENT_TIME, LOCALTIME"},
			{Name: "BYTEA", Parse: Yes, Exec: Yes, SQL: "CREATE TABLE t (a BYTEA)"},
			{Name: "NUMERIC / DECIMAL", Parse: Yes, Exec: Partial,
				Note: "stored as double precision; exact decimal arithmetic is pending",
				SQL:  "CREATE TABLE t (a NUMERIC(10, 2))"},
			{Name: "UUID", Parse: Yes, Exec: Partial,
				Note: "accepted and stored as text; no validation",
				SQL:  "CREATE TABLE t (a UUID)"},
		},
	},
	{
		Name: "Transactions and sessions",
		Features: []Feature{
			{Name: "BEGIN / COMMIT / ROLLBACK", Parse: Yes, Exec: Yes,
				Note: "via database/sql Tx; rollback discards inserts, updates and deletes alike"},
			{Name: "Isolation levels", Parse: Yes, Exec: Partial,
				Note: "READ COMMITTED and REPEATABLE READ honoured from sql.TxOptions; SERIALIZABLE is accepted but behaves as REPEATABLE READ, since write-skew detection is not implemented"},
			{Name: "Read-only transactions", Parse: Yes, Exec: Yes,
				Note: "sql.TxOptions.ReadOnly refuses data-modifying statements with SQLSTATE 25006"},
			{Name: "Failed transaction state", Parse: Yes, Exec: Yes,
				Note: "a statement error aborts the transaction; later commands are refused with 25P02 until rollback"},
			{Name: "MVCC snapshot isolation", Parse: Yes, Exec: Yes,
				Note: "readers never block writers; a write conflict is reported as SQLSTATE 40001"},
			{Name: "Savepoints", Parse: Planned, Exec: Planned},
			{Name: "VACUUM", Parse: Planned, Exec: Planned,
				Note: "the storage layer can reclaim dead row versions, but no statement exposes it and nothing triggers it automatically yet",
				SQL:  "VACUUM t"},
		},
	},
	{
		Name: "Driver and diagnostics",
		Features: []Feature{
			{Name: "Named in-memory instances", Parse: Yes, Exec: Yes,
				Note: "one data source name per test; Drop releases an instance"},
			{Name: "Multi-statement Exec", Parse: Yes, Exec: Yes,
				Note:  "a fixture can be one semicolon-separated batch",
				Setup: probeTable,
				SQL:   "INSERT INTO t (a) VALUES (1); INSERT INTO t (a) VALUES (2)"},
			{Name: "Prepared statements", Parse: Yes, Exec: Yes,
				Note: "bound once, executed repeatedly"},
			{Name: "Column type introspection", Parse: Yes, Exec: Yes,
				Note: "ScanType, DatabaseTypeName and Nullable for ORMs"},
			// The probe parses today because it is only a schema-qualified
			// name; what is missing is the views themselves.
			{Name: "information_schema", Parse: Yes, Exec: Planned,
				Note: "tables, columns, table_constraints and key_column_usage as read-only views over the catalog; ORMs query these when migrating",
				SQL:  "SELECT table_name FROM information_schema.tables"},
			// Like information_schema, the name already parses; only the
			// views are missing.
			{Name: "pg_catalog", Parse: Yes, Exec: Planned,
				Note: "the subset ORMs actually read, such as pg_class and pg_attribute",
				SQL:  "SELECT relname FROM pg_catalog.pg_class"},
			{Name: "Context cancellation", Parse: Yes, Exec: Yes,
				Note: "checked inside the operator loop, so a running query stops"},
			{Name: "SQLSTATE on every error", Parse: Yes, Exec: Yes,
				Note: "errors satisfy interface{ SQLState() string }, as pgx and lib/pq do"},
			// Partial rather than yes: what is on disk is correct and a crash
			// during a commit loses at most that commit, but the log is
			// compacted only when the database is closed, and nothing stops a
			// second process opening the same directory.
			{Name: "File-backed storage", Parse: Yes, Exec: Partial,
				Note: "write-ahead log, fsync on commit, compacted at close; open with file:./demo.db"},
		},
	},
	{
		Name: "Lexical",
		Features: []Feature{
			{Name: "Comments", Parse: Yes, Exec: Yes,
				Note: "-- to end of line, and nestable /* */",
				SQL:  "SELECT 1 -- trailing\n/* block /* nested */ */"},
			{Name: "Quoted identifiers", Parse: Yes, Exec: Yes,
				Note:  "case preserving, doubled quote escapes",
				Setup: []string{`CREATE TABLE "User" ("Name" TEXT)`},
				SQL:   `SELECT "Name" FROM "User"`},
			{Name: "String literals", Parse: Yes, Exec: Yes,
				Note: "doubled-quote escapes and E-prefixed backslash escapes",
				SQL:  `SELECT 'it''s', E'a\nb'`},
			{Name: "Dollar quoting", Parse: Yes, Exec: Yes,
				Note: "$$ and $tag$, contents taken verbatim",
				SQL:  "SELECT $tag$ raw 'text' $tag$"},
			{Name: "Positional errors", Parse: Yes, Exec: Yes,
				Note: "every error carries a byte offset into the statement"},
		},
	},
}

// Badges renders the shields.io badge row for the README.
//
// The feature count comes from Summary rather than being written by hand, so it
// cannot drift from the registry the way a hard-coded number would — and the
// README test regenerates it along with the table.
func Badges(module, repo, goVersion string) string {
	yes, partial, _, _ := Summary()

	badge := func(label, message, colour, link string) string {
		img := fmt.Sprintf("https://img.shields.io/badge/%s-%s-%s?style=flat-square",
			escapeBadge(label), escapeBadge(message), colour)
		if link == "" {
			return fmt.Sprintf("![%s](%s)", label, img)
		}
		return fmt.Sprintf("[![%s](%s)](%s)", label, img, link)
	}

	return strings.Join([]string{
		fmt.Sprintf("[![Go Reference](https://pkg.go.dev/badge/%s.svg)](https://pkg.go.dev/%s)", module, module),
		fmt.Sprintf("[![CI](https://github.com/%s/actions/workflows/ci.yml/badge.svg)](https://github.com/%s/actions/workflows/ci.yml)", repo, repo),
		fmt.Sprintf("[![Go Report Card](https://goreportcard.com/badge/%s?style=flat-square)](https://goreportcard.com/report/%s)", module, module),
		badge("go", goVersion+"+", "00ADD8", ""),
		badge("license", "Apache--2.0", "blue", "LICENSE"),
		badge("SQL features", fmt.Sprintf("%d supported", yes+partial), "success", "#compatibility"),
		badge("dependencies", "0", "success", ""),
	}, "\n") + "\n"
}

// escapeBadge encodes the characters shields.io treats specially in a path
// segment: a dash and an underscore are each doubled, and a space becomes an
// underscore.
func escapeBadge(s string) string {
	s = strings.ReplaceAll(s, "-", "--")
	s = strings.ReplaceAll(s, "_", "__")
	return strings.ReplaceAll(s, " ", "_")
}

// Markdown renders the whole matrix.
func Markdown() string {
	var b strings.Builder

	b.WriteString("| | Feature | Parses | Executes | Notes |\n")
	b.WriteString("|---|---|:---:|:---:|---|\n")
	for _, g := range Groups {
		fmt.Fprintf(&b, "| | **%s** | | | |\n", g.Name)
		for _, f := range g.Features {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
				mark[f.Overall()], f.Name, mark[f.Parse], mark[f.Exec], f.Note)
		}
	}

	b.WriteString("\n")
	b.WriteString("✅ supported &nbsp;&nbsp; 🟡 partial &nbsp;&nbsp; ⬜ planned &nbsp;&nbsp; ❌ out of scope\n")
	return b.String()
}

// Summary counts features by overall status, for the README's headline.
func Summary() (yes, partial, planned, no int) {
	for _, g := range Groups {
		for _, f := range g.Features {
			switch f.Overall() {
			case Yes:
				yes++
			case Partial:
				partial++
			case Planned:
				planned++
			default:
				no++
			}
		}
	}
	return
}
