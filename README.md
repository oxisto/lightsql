# lightsql

<!-- BEGIN GENERATED BADGES -->
[![Go Reference](https://pkg.go.dev/badge/github.com/oxisto/lightsql.svg)](https://pkg.go.dev/github.com/oxisto/lightsql)
[![CI](https://github.com/oxisto/lightsql/actions/workflows/ci.yml/badge.svg)](https://github.com/oxisto/lightsql/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/oxisto/lightsql?style=flat-square)](https://goreportcard.com/report/github.com/oxisto/lightsql)
![go](https://img.shields.io/badge/go-1.26+-00ADD8?style=flat-square)
[![license](https://img.shields.io/badge/license-Apache----2.0-blue?style=flat-square)](LICENSE)
[![SQL features](https://img.shields.io/badge/SQL_features-68_supported-success?style=flat-square)](#compatibility)
![dependencies](https://img.shields.io/badge/dependencies-0-success?style=flat-square)
<!-- END GENERATED BADGES -->

A small, embeddable SQL engine for Go that speaks the PostgreSQL dialect and plugs
straight into `database/sql`. Run it entirely in memory for tests, or point it at a
directory for a small file-backed deployment.

> **Status: the core engine works.** DDL, `INSERT`, `UPDATE`, `DELETE`,
> `RETURNING` and `SELECT` with joins, `GROUP BY`, subqueries and `ON CONFLICT`
> all run end to end through `database/sql`, with `NOT NULL`, `PRIMARY KEY`,
> `UNIQUE`, `DEFAULT`, `CHECK` and foreign keys enforced, and real transactions
> on MVCC — `Begin`, `Commit` and `Rollback` work, and `sql.TxOptions` isolation
> levels are honoured rather than ignored. A database can be kept in a directory
> and survives a restart.
> Still missing: `UNION` and its siblings, correlated subqueries, `INTERVAL`
> arithmetic, and the `information_schema` and `pg_catalog` views that ORMs read
> when they migrate.
> Do not take this paragraph's word for any of it. The compatibility matrix below
> is generated from the code, and every row is backed by a probe that is actually
> run — see [Compatibility](#compatibility).

```go
import (
    "database/sql"

    _ "github.com/oxisto/lightsql/driver"
)

func TestOrders(t *testing.T) {
    // One named instance per test gives full isolation with no setup.
    db, err := sql.Open("lightsql", "TestOrders")
    if err != nil {
        t.Fatal(err)
    }
    defer db.Close()

    // ... exercise the code under test against a real SQL engine.
}
```

A name selects an instance **inside the current process**. Two processes opening
`"TestOrders"` get two separate, empty databases — the name is a key in a map,
not a path to anything shared. That is what you want for tests, where it means
one line of setup and no cleanup. For anything that has to outlive one process,
or be reached from more than one, point the name at a directory instead:
`sql.Open("lightsql", "file:./demo.db")`.

## Why

Testing code that talks to PostgreSQL usually means one of three bad trades: mock the
database and test nothing real, run a container and pay for it on every `go test`, or
swap in SQLite and quietly test a different dialect.

lightsql aims at the fourth option — a real SQL engine, in-process, with PostgreSQL
semantics — for two use cases specifically:

- **Unit tests.** No setup, no containers, no credentials. One data source name per
  test and the tests are isolated from each other.
- **Small demo deployments.** A single-binary application with a modest amount of
  data that should survive a restart, without operating a database server.

It is explicitly **not** a general-purpose database. See [Non-goals](#non-goals).

## Design

The pipeline is conventional and deliberately so:

```
SQL text ──► scanner ──► parser ──► binder ──► optimizer ──► executor ──► rows
             tokens      AST        plan       plan          operators
```

A few decisions are worth calling out, because they are the ones that shape everything
else:

- **Typed AST.** Every construct has its own node type, in the style of `go/ast`. The
  common alternative — one generic node with a token, a lexeme and a slice of children
  — pushes the grammar out of the type system and turns every consumer into positional
  child access.
- **A real expression grammar.** Operator precedence lives in one table consumed by a
  precedence-climbing parser, so `a * b + c` and `x OR y AND z` nest correctly, and
  `WHERE`, `HAVING` and the select list all share it.
- **Positions everywhere.** Every token and node carries a byte offset, and every error
  carries a SQLSTATE code and a position. Errors implement
  `interface{ SQLState() string }`, the same de-facto contract `pgx` and `lib/pq`
  satisfy, so `errors.As` handling for things like unique violations keeps working.
- **A typed value, not `any`.** A column value is a 32-byte struct with a small kind
  tag. Scalars do not allocate, `NULL` is a kind rather than a nil interface, and
  comparisons return a three-valued boolean, so `NULL = NULL` is `UNKNOWN` by
  construction rather than by remembering to check.
- **MVCC.** Rows are versioned with creating and deleting transaction ids, and each
  transaction reads from a snapshot. Rollback is marking a transaction aborted, readers
  never block writers, and `sql.TxOptions` isolation levels map onto PostgreSQL's.
- **Names resolve once.** The binder turns every column reference into an ordinal.
  Nothing below the binder compares column names as strings.

## Persistence

A data source name that is a plain name selects an in-memory instance held in a map
inside the current process. Nothing about it reaches the filesystem, so a second
process using the same name gets its own empty database rather than joining the
first — a stronger constraint than "the data does not survive a restart", and the
one to check a design against before building a tool that expects to share.

Point the name at a directory instead and each committed transaction is appended to a
write-ahead log there first, so the database comes back after a restart.

```go
db, err := sql.Open("lightsql", "file:./demo.db")          // fsync on every commit
db, err := sql.Open("lightsql", "file:./demo.db?fsync=off") // faster, loses the guarantee
```

```
demo.db/
  wal              every committed change, oldest first
```

The log is logical — row values and the text of DDL statements — rather than physical
pages, so the format does not change every time an internal structure does. DDL is
recorded as the statement that was run, because replaying it rebuilds the catalog
exactly, including the `DEFAULT` expressions and `CHECK` predicates the catalog itself
stores as syntax.

**One frame holds one transaction**, length-prefixed with a CRC32 over its records. A
torn write therefore fails its checksum and the whole transaction is discarded, so
recovery never applies half of one. Recovery is also a repair: the partial tail is
truncated, without which the next commit would land after unreadable bytes and be lost
at the restart after that.

Closing the database rewrites the log as the state rather than the history that produced
it, so a table updated a million times does not replay a million versions at startup.

Two limits worth knowing before pointing something at a directory: the log is compacted
only when the database is closed, and nothing stops a second process opening the same
directory — there is no lock file yet.

This means the working set must fit in memory. That is a deliberate trade for the
target use cases, not an oversight.

## Compatibility

The table below is generated from `internal/features`, and a test fails if it drifts
from the code.

Each row is backed by a probe statement that is **executed**, not just inspected: a row
claiming support whose probe fails is a test failure, and so is a row claiming *no*
support whose probe succeeds. The matrix is therefore checked in both directions rather
than asserted — it cannot quietly overstate or understate what works.

**Parses** covers the scanner, parser and AST. **Executes** covers the binder, planner
and executor. The leftmost column is what a user actually gets, which is the lesser of
the two.

<!-- BEGIN GENERATED COMPATIBILITY -->

| | Feature | Parses | Executes | Notes |
|---|---|:---:|:---:|---|
| | **Data definition** | | | |
| ✅ | CREATE TABLE | ✅ | ✅ |  |
| ✅ | CREATE TABLE IF NOT EXISTS | ✅ | ✅ |  |
| ✅ | Column constraints | ✅ | ✅ | NOT NULL, PRIMARY KEY, UNIQUE, DEFAULT, CHECK and REFERENCES are all enforced |
| ✅ | Table constraints | ✅ | ✅ | PRIMARY KEY, UNIQUE, CHECK and FOREIGN KEY, with keys over several columns requiring the combination to be unique |
| ✅ | DEFAULT values | ✅ | ✅ | any constant expression; an omitted column takes it, an explicit NULL overrides it |
| ✅ | CHECK constraints | ✅ | ✅ | column and table level, enforced on insert and update; satisfied by true or unknown, so a NULL does not violate one |
| ✅ | Foreign keys | ✅ | ✅ | column and table level, single and multi-column; a NULL in the key is unconstrained, as MATCH SIMPLE requires |
| ✅ | Referential actions | ✅ | ✅ | ON DELETE and ON UPDATE with CASCADE, RESTRICT, NO ACTION, SET NULL and SET DEFAULT; cascades are part of the transaction, recurse past the immediate children, and terminate on a cyclic reference |
| ✅ | DROP TABLE | ✅ | ✅ | several tables at once, IF EXISTS, and RESTRICT which is the default: a table another one references is kept, unless both are named in the same statement. CASCADE parses but is refused rather than half-performed. Like every DDL statement it is not transactional, so a rolled back drop does not bring the table back |
| 🟡 | ALTER TABLE | 🟡 | 🟡 | ADD COLUMN, ALTER COLUMN SET and DROP NOT NULL, RENAME TO and RENAME COLUMN. Adding a column does not rewrite the rows already stored: they stay shorter than the table and read it as its DEFAULT, or NULL, which is what PostgreSQL calls a missing value. NOT NULL without a DEFAULT is refused, since every stored row would violate it; SET NOT NULL afterwards is how a backfilled column is tightened, and it is checked against the rows already there. A foreign key survives a rename, holding a table pointer and ordinals rather than names, but renaming a column that a CHECK, a DEFAULT or a partial index predicate names is refused, since those are stored as syntax. DROP COLUMN and a type change are not supported: neither can be served by a missing value. SET and DROP DEFAULT are not supported yet |
| 🟡 | CREATE INDEX | ✅ | 🟡 | UNIQUE and partial indexes are enforced as constraints, including a partial one whose predicate decides which rows it covers. A plain index is recorded but builds no structure and is never chosen, because there is no index selection in the planner yet -- so it costs nothing and speeds nothing up. DROP INDEX is supported; expression indexes and a per-column sort order are not |
| ⬜ | CREATE VIEW | ⬜ | ⬜ |  |
| ⬜ | CREATE SCHEMA | ⬜ | ⬜ |  |
| ✅ | Sequences and SERIAL | ✅ | ✅ | an omitted SERIAL column is filled from a per-column sequence |
| 🟡 | Identity columns | ✅ | 🟡 | GENERATED ALWAYS and GENERATED BY DEFAULT AS IDENTITY on an integer column, backed by the same per-column sequence as SERIAL and implying NOT NULL. ALWAYS refuses a statement that supplies its own value, as PostgreSQL does, but OVERRIDING SYSTEM VALUE is not implemented, so there is no way to override it. Sequence options such as START WITH are refused rather than ignored |
| | **Data manipulation** | | | |
| ✅ | INSERT ... VALUES | ✅ | ✅ | including multi-row VALUES |
| ✅ | INSERT ... SELECT | ✅ | ✅ | the source may be any query, and its rows go through the same serial, DEFAULT, CHECK and RETURNING handling as VALUES. Reading the table being written is safe, since a scan takes its rows when the operator is built |
| ✅ | RETURNING | ✅ | ✅ | on INSERT, UPDATE and DELETE; sees generated serial values |
| ✅ | UPDATE | ✅ | ✅ | assignments all read the original row, so SET a = b, b = a swaps |
| ✅ | DELETE | ✅ | ✅ | row order is preserved for the rows that remain |
| ✅ | ON CONFLICT | ✅ | ✅ | DO NOTHING, with or without a target, and DO UPDATE with an optional WHERE. The update sees the stored row by table name and the proposed one as excluded. A target must be covered by a primary key, unique constraint or total unique index, since one nothing enforces would never detect a collision. A skip reports zero rows affected |
| ⬜ | TRUNCATE | ⬜ | ⬜ |  |
| | **Queries** | | | |
| ✅ | SELECT list, aliases | ✅ | ✅ | AS is optional |
| ✅ | SELECT without FROM | ✅ | ✅ |  |
| ✅ | WHERE | ✅ | ✅ |  |
| ✅ | LIMIT / OFFSET | ✅ | ✅ | either order, LIMIT ALL accepted |
| ✅ | Table aliases | ✅ | ✅ | an alias replaces the table name, as in PostgreSQL |
| ✅ | Inner and outer joins | ✅ | ✅ | INNER, LEFT, RIGHT, FULL and CROSS, with ON or USING; a comma in FROM is a cross join; nested loop, so no index is used yet |
| ✅ | JOIN ... USING | ✅ | ✅ | the pair is merged into one column, so it is unambiguous unqualified and appears once in SELECT * |
| ✅ | GROUP BY / HAVING | ✅ | ✅ | groups on columns or expressions; NULLs form one group; HAVING may use an aggregate the select list does not |
| 🟡 | Aggregate functions | ✅ | 🟡 | count, sum, avg, min and max, each with DISTINCT; count is 0 over no rows and the rest are NULL. Other aggregates are pending |
| ✅ | ORDER BY | ✅ | ✅ | ASC/DESC, NULLS FIRST/LAST, output aliases, select-list positions, and expressions over unselected columns |
| ✅ | DISTINCT / DISTINCT ON | ✅ | ✅ | compares the output row, and treats NULLs as equal; DISTINCT ON keeps the first row per key, so ORDER BY decides which. Unlike PostgreSQL, ORDER BY on an unselected column is accepted rather than rejected |
| 🟡 | Subqueries | ✅ | 🟡 | scalar, IN, EXISTS and derived tables, which must have an alias. A scalar subquery is NULL over no rows and an error over more than one. Only uncorrelated subqueries are supported: one that references the outer query, and LATERAL, are both rejected rather than mis-resolved |
| ⬜ | UNION / INTERSECT / EXCEPT | ⬜ | ⬜ |  |
| ⬜ | Common table expressions | ⬜ | ⬜ | WITH, including RECURSIVE |
| ❌ | Window functions | ❌ | ❌ | out of scope for v1 |
| | **Expressions** | | | |
| ✅ | Operator precedence | ✅ | ✅ | full PostgreSQL precedence table, including left-associative ^ |
| ✅ | Comparison and logic | ✅ | ✅ | three-valued logic throughout |
| ✅ | IS NULL / IS DISTINCT FROM | ✅ | ✅ |  |
| ✅ | String concatenation | ✅ | ✅ | NULL propagates, as in PostgreSQL |
| ✅ | Parameter placeholders | ✅ | ✅ | $1 and ?, not mixed in one statement; the type is inferred from context |
| ✅ | BETWEEN / IN / LIKE | ✅ | ✅ | including the negated forms. BETWEEN is inclusive and rewritten to a pair of comparisons; LIKE supports % and _ with backslash escaping, and is anchored, so it matches the whole string. IN follows SQL's three-valued rule: without a match, a NULL among the candidates makes the answer unknown rather than false, so NOT IN over a NULL returns no rows. ILIKE and an explicit ESCAPE clause are not supported |
| ✅ | CASE | ✅ | ✅ | simple and searched forms; the simple form is rewritten to the searched one, so both take one path. Only a true condition fires an arm, no match without ELSE is NULL, and the branches must share a type so a result column cannot change type from row to row |
| ✅ | CAST | ✅ | ✅ | both CAST(x AS t) and x::t |
| 🟡 | Scalar functions | ✅ | 🟡 | coalesce, nullif, now, lower, upper, length, trim, abs and round. NULL propagates for all but coalesce and nullif, and coalesce stops at the first argument that answers. Argument types are checked at bind time, so lower(1) is rejected rather than reading an integer as text. The library is small and grows on demand |
| ❌ | Arrays | ❌ | ❌ | out of scope for v1 |
| ✅ | JSONB | ✅ | ✅ | canonicalised on store; scans as []byte |
| ✅ | JSON | ✅ | ✅ | kept exactly as written, unlike jsonb |
| | **Types** | | | |
| ✅ | Integer types | ✅ | ✅ | SMALLINT, INT, BIGINT stored as int64 |
| ✅ | Floating point | ✅ | ✅ | REAL and DOUBLE PRECISION stored as float64 |
| ✅ | Character types | ✅ | ✅ | TEXT, VARCHAR(n), CHARACTER VARYING(n), CHAR; length is recorded but not enforced |
| ✅ | BOOLEAN | ✅ | ✅ |  |
| 🟡 | Date and time | ✅ | 🟡 | columns, time.Time arguments and ISO 8601 literals, with either a space or a T separator. A zone offset is honoured by timestamptz and dropped by timestamp, as "without time zone" requires. A bare literal takes its type from the column it is compared or assigned to. INTERVAL is pending, and the non-ISO date styles PostgreSQL accepts are deliberately not, since 01/02/2024 has no reading that is right in both conventions |
| 🟡 | Current date and time | ✅ | 🟡 | now(), CURRENT_TIMESTAMP, LOCALTIMESTAMP, CURRENT_DATE, CURRENT_TIME and LOCALTIME, all reporting the transaction start so that one transaction cannot disagree with itself. CURRENT_TIME is a plain time rather than PostgreSQL's zoned one, and a precision argument such as CURRENT_TIMESTAMP(0) is refused rather than ignored |
| ✅ | BYTEA | ✅ | ✅ |  |
| 🟡 | NUMERIC / DECIMAL | ✅ | 🟡 | stored as double precision; exact decimal arithmetic is pending |
| 🟡 | UUID | ✅ | 🟡 | accepted and stored as text; no validation |
| | **Transactions and sessions** | | | |
| ✅ | BEGIN / COMMIT / ROLLBACK | ✅ | ✅ | via database/sql Tx; rollback discards inserts, updates and deletes alike |
| 🟡 | Isolation levels | ✅ | 🟡 | READ COMMITTED and REPEATABLE READ honoured from sql.TxOptions; SERIALIZABLE is accepted but behaves as REPEATABLE READ, since write-skew detection is not implemented |
| ✅ | Read-only transactions | ✅ | ✅ | sql.TxOptions.ReadOnly refuses data-modifying statements with SQLSTATE 25006 |
| ✅ | Failed transaction state | ✅ | ✅ | a statement error aborts the transaction; later commands are refused with 25P02 until rollback |
| ✅ | MVCC snapshot isolation | ✅ | ✅ | readers never block writers; a write conflict is reported as SQLSTATE 40001 |
| ⬜ | Savepoints | ⬜ | ⬜ |  |
| ⬜ | VACUUM | ⬜ | ⬜ | the storage layer can reclaim dead row versions, but no statement exposes it and nothing triggers it automatically yet |
| | **Driver and diagnostics** | | | |
| ✅ | Named in-memory instances | ✅ | ✅ | one data source name per test; Drop releases an instance |
| ✅ | Multi-statement Exec | ✅ | ✅ | a fixture can be one semicolon-separated batch |
| ✅ | Prepared statements | ✅ | ✅ | bound once, executed repeatedly |
| ✅ | Column type introspection | ✅ | ✅ | ScanType, DatabaseTypeName and Nullable for ORMs |
| ⬜ | information_schema | ✅ | ⬜ | tables, columns, table_constraints and key_column_usage as read-only views over the catalog; ORMs query these when migrating |
| ⬜ | pg_catalog | ✅ | ⬜ | the subset ORMs actually read, such as pg_class and pg_attribute |
| ✅ | Context cancellation | ✅ | ✅ | checked inside the operator loop, so a running query stops |
| ✅ | SQLSTATE on every error | ✅ | ✅ | errors satisfy interface{ SQLState() string }, as pgx and lib/pq do |
| 🟡 | File-backed storage | ✅ | 🟡 | write-ahead log, fsync on commit, compacted at close; open with file:./demo.db |
| | **Lexical** | | | |
| ✅ | Comments | ✅ | ✅ | -- to end of line, and nestable /* */ |
| ✅ | Quoted identifiers | ✅ | ✅ | case preserving, doubled quote escapes |
| ✅ | String literals | ✅ | ✅ | doubled-quote escapes and E-prefixed backslash escapes |
| ✅ | Dollar quoting | ✅ | ✅ | $$ and $tag$, contents taken verbatim |
| ✅ | Positional errors | ✅ | ✅ | every error carries a byte offset into the statement |

✅ supported &nbsp;&nbsp; 🟡 partial &nbsp;&nbsp; ⬜ planned &nbsp;&nbsp; ❌ out of scope

<!-- END GENERATED COMPATIBILITY -->

## Non-goals

- **A general-purpose database.** No page cache, no on-disk B-trees, no replication.
  Data must fit in memory.
- **The PostgreSQL wire protocol.** lightsql is a Go library, not a server. `psql`
  cannot connect to it. The engine avoids `database/sql` types internally so a wire
  front end stays possible, but it is not planned.
- **Multi-process access.** One process owns a data directory at a time, enforced by a
  lock file.
- **Peak performance.** Correctness and dialect fidelity come first. The target is
  "fast enough that a test suite does not notice", not competing with a real server.

## Dependencies

The root module depends on the standard library and `golang.org/x/*`, and nothing else.
A test enforces this, so adding lightsql to a project does not drag a dependency tree
into it. ORM compatibility suites live in a separate `compat/` module, so their
dependencies stay out of yours.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the layout, the test strategy and the
workflow for adding a SQL feature. [CLAUDE.md](CLAUDE.md) documents the architecture and
the invariants that hold across packages.

## License

See [LICENSE](LICENSE).
