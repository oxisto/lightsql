# lightsql

<!-- BEGIN GENERATED BADGES -->
[![Go Reference](https://pkg.go.dev/badge/github.com/oxisto/lightsql.svg)](https://pkg.go.dev/github.com/oxisto/lightsql)
[![CI](https://github.com/oxisto/lightsql/actions/workflows/ci.yml/badge.svg)](https://github.com/oxisto/lightsql/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/oxisto/lightsql?style=flat-square)](https://goreportcard.com/report/github.com/oxisto/lightsql)
![go](https://img.shields.io/badge/go-1.26+-00ADD8?style=flat-square)
[![license](https://img.shields.io/badge/license-Apache----2.0-blue?style=flat-square)](LICENSE)
[![SQL features](https://img.shields.io/badge/SQL_features-53_supported-success?style=flat-square)](#compatibility)
![dependencies](https://img.shields.io/badge/dependencies-0-success?style=flat-square)
<!-- END GENERATED BADGES -->

A small, embeddable SQL engine for Go that speaks the PostgreSQL dialect and plugs
straight into `database/sql`. Run it entirely in memory for tests, or point it at a
directory for a small file-backed deployment.

> **Status: the core engine works.** `CREATE TABLE`, `INSERT`, `SELECT ... WHERE
> ... ORDER BY ... LIMIT`, `UPDATE`, `DELETE` and `RETURNING` all run end to end
> through `database/sql`, with `NOT NULL`, `PRIMARY KEY`, `UNIQUE`, `DEFAULT` and
> `CHECK` and foreign keys enforced, and real transactions on MVCC — `Begin`,
> `Commit` and `Rollback` work, and `sql.TxOptions` isolation levels are honoured
> rather than ignored. Joins, aggregates and file-backed storage are not
> implemented yet.
> The compatibility matrix below is generated from the code, and every row is backed by
> a probe that is actually run — see [Compatibility](#compatibility).

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

In-memory is the source of truth. When a directory is configured, each committed
transaction appends a record to a write-ahead log, and a periodic checkpoint writes a
full snapshot and truncates the log.

```
mydb/
  LOCK             flock'd, so only one process opens the directory
  snapshot.0007    full catalog and rows
  wal.0007         commit records since snapshot 7
```

The log is logical (row changes and DDL) rather than physical pages, encoded as
length-prefixed varints with a CRC per record. Recovery loads the snapshot and replays
committed records, discarding a torn trailing record.

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
| ✅ | Referential actions | ✅ | ✅ | ON DELETE and ON UPDATE with CASCADE, RESTRICT, NO ACTION, SET NULL and SET DEFAULT; cascades are part of the transaction |
| ⬜ | DROP TABLE | ⬜ | ⬜ |  |
| ⬜ | ALTER TABLE | ⬜ | ⬜ |  |
| ⬜ | CREATE INDEX | ⬜ | ⬜ |  |
| ⬜ | CREATE VIEW | ⬜ | ⬜ |  |
| ⬜ | CREATE SCHEMA | ⬜ | ⬜ |  |
| ✅ | Sequences and SERIAL | ✅ | ✅ | an omitted SERIAL column is filled from a per-column sequence |
| | **Data manipulation** | | | |
| ✅ | INSERT ... VALUES | ✅ | ✅ | including multi-row VALUES |
| ⬜ | INSERT ... SELECT | ✅ | ⬜ |  |
| ✅ | RETURNING | ✅ | ✅ | on INSERT, UPDATE and DELETE; sees generated serial values |
| ✅ | UPDATE | ✅ | ✅ | assignments all read the original row, so SET a = b, b = a swaps |
| ✅ | DELETE | ✅ | ✅ | row order is preserved for the rows that remain |
| ⬜ | ON CONFLICT | ⬜ | ⬜ |  |
| ⬜ | TRUNCATE | ⬜ | ⬜ |  |
| | **Queries** | | | |
| ✅ | SELECT list, aliases | ✅ | ✅ | AS is optional |
| ✅ | SELECT without FROM | ✅ | ✅ |  |
| ✅ | WHERE | ✅ | ✅ |  |
| ✅ | LIMIT / OFFSET | ✅ | ✅ | either order, LIMIT ALL accepted |
| ✅ | Table aliases | ✅ | ✅ | an alias replaces the table name, as in PostgreSQL |
| ✅ | Inner and outer joins | ✅ | ✅ | INNER, LEFT, RIGHT, FULL and CROSS, with ON or USING; a comma in FROM is a cross join; nested loop, so no index is used yet |
| ✅ | JOIN ... USING | ✅ | ✅ | the pair is merged into one column, so it is unambiguous unqualified and appears once in SELECT * |
| ⬜ | GROUP BY / HAVING | ✅ | ⬜ |  |
| ⬜ | Aggregate functions | ✅ | ⬜ | parsed generically; the function library is pending |
| ✅ | ORDER BY | ✅ | ✅ | ASC/DESC, NULLS FIRST/LAST, output aliases, select-list positions, and expressions over unselected columns |
| ⬜ | DISTINCT / DISTINCT ON | ✅ | ⬜ |  |
| ⬜ | Subqueries | ✅ | ⬜ | scalar, IN, EXISTS, and derived tables |
| ⬜ | UNION / INTERSECT / EXCEPT | ⬜ | ⬜ |  |
| ⬜ | Common table expressions | ⬜ | ⬜ | WITH, including RECURSIVE |
| ❌ | Window functions | ❌ | ❌ | out of scope for v1 |
| | **Expressions** | | | |
| ✅ | Operator precedence | ✅ | ✅ | full PostgreSQL precedence table, including left-associative ^ |
| ✅ | Comparison and logic | ✅ | ✅ | three-valued logic throughout |
| ✅ | IS NULL / IS DISTINCT FROM | ✅ | ✅ |  |
| ✅ | String concatenation | ✅ | ✅ | NULL propagates, as in PostgreSQL |
| ✅ | Parameter placeholders | ✅ | ✅ | $1 and ?, not mixed in one statement; the type is inferred from context |
| ⬜ | BETWEEN / IN / LIKE | ✅ | ⬜ | including the negated forms |
| ⬜ | CASE | ✅ | ⬜ | simple and searched forms |
| ✅ | CAST | ✅ | ✅ | both CAST(x AS t) and x::t |
| ⬜ | Scalar functions | ✅ | ⬜ | parsed generically; the function library is pending |
| ❌ | Arrays | ❌ | ❌ | out of scope for v1 |
| ✅ | JSONB | ✅ | ✅ | canonicalised on store; scans as []byte |
| ✅ | JSON | ✅ | ✅ | kept exactly as written, unlike jsonb |
| | **Types** | | | |
| ✅ | Integer types | ✅ | ✅ | SMALLINT, INT, BIGINT stored as int64 |
| ✅ | Floating point | ✅ | ✅ | REAL and DOUBLE PRECISION stored as float64 |
| ✅ | Character types | ✅ | ✅ | TEXT, VARCHAR(n), CHARACTER VARYING(n), CHAR; length is recorded but not enforced |
| ✅ | BOOLEAN | ✅ | ✅ |  |
| 🟡 | Date and time | ✅ | 🟡 | columns and time.Time arguments work; date and time literals are pending |
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
| ⬜ | File-backed storage | ⬜ | ⬜ | WAL plus periodic snapshot |
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
