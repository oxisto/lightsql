# Contributing to lightsql

Thanks for taking an interest. This document covers the layout, how the project is
tested, and what a change is expected to include.

## Getting started

```sh
git clone https://github.com/oxisto/lightsql
cd lightsql
go test ./...
```

There is nothing to install. The root module depends on the standard library and
`golang.org/x/*` only, and a test enforces that.

## Layout

```
.                      public API: connector, options, errors
driver/                database/sql adapter — a thin shim, no logic
internal/token/        token kinds and source positions
internal/scanner/      SQL text  → tokens
internal/parser/       tokens    → typed AST (recursive descent + Pratt expressions)
internal/ast/          the AST and its S-expression printer
internal/types/        Value, three-valued logic, comparison and hashing
internal/catalog/      schemas, tables, columns, constraints, sequences
internal/binder/       AST + catalog → plan (names resolved to ordinals)
internal/plan/         logical plan and optimizer rules
internal/exec/         physical operators and compiled expressions
internal/storage/      MVCC heap, indexes, transaction manager
internal/wal/          write-ahead log and snapshots
internal/pgerr/        SQLSTATE codes and the structured error type
internal/features/     the compatibility registry that generates the README table
compat/                separate module: ORM compatibility suites and benchmarks
```

The pipeline is strictly one-directional. A package never imports one further down the
list than itself.

## Architecture rules

[CLAUDE.md](CLAUDE.md) lists the invariants in full. The ones that most often catch
people out:

- **The AST is typed and immutable.** One node type per construct, every node has a
  position, and nothing rewrites a node while walking it.
- **A column value is `types.Value`, never `any`.** No reflection and no `fmt.Sprintf`
  on a per-row path.
- **Comparisons are three-valued.** `types.Compare` is a total order for sorting and
  indexing; `types.Eq` and friends implement SQL semantics where NULL poisons the
  result. They are different functions on purpose.
- **Errors carry a SQLSTATE and a position.** Build them with `pgerr`.

## Testing

Correctness is the whole product, so tests are weighted accordingly.

| Kind | Where | What it protects |
|---|---|---|
| Table tests | every package | ordinary behaviour and edge cases |
| Whole-tree assertions | `internal/parser` | precedence and associativity, via `ast.Sprint` |
| Positional error tests | scanner, parser, binder | every failure names a SQLSTATE and an offset |
| Feature probes | `internal/features` | the compatibility matrix matches the code |
| Fuzzing | scanner, parser | no panic and no hang on any input |
| Truth tables | `internal/types` | three-valued logic against SQL's definitions |
| Race tests | `internal/storage` | snapshot isolation invariants under concurrency |
| Crash injection | `internal/wal` | recovery from a log truncated at any offset |
| Differential tests | `parity` build tag | results and SQLSTATEs against a real PostgreSQL |
| ORM suites | `compat/` | gorm, sqlx, ent, sqlc, gorp actually work |

Run the differential suite against a real server with:

```sh
LIGHTSQL_PARITY_DSN='postgres://...' go test -tags parity ./...
```

Guidelines:

- Assert on the **whole** parsed tree rather than individual fields. A parser that drops
  an operand still passes a test that only checks the operator.
- Every new error path gets a test asserting its **position**, not just its message.
- When you fix a bug, add the test that would have caught it, and say in a comment what
  it protects against.

## Adding a SQL feature

Work through the pipeline in order — token, scanner, AST, parser, binder, plan, exec —
then update `internal/features` with an entry and a probe statement and regenerate the
README:

```sh
go test ./internal/features -update
```

The compatibility table in the README is generated from that registry and verified by a
test, so a feature added without updating it fails CI. This is deliberate: the matrix is
the project's main promise to its users, and a hand-maintained one drifts within weeks.

One subtlety when adding a keyword: decide whether it must be **reserved**. An
unreserved word appearing where an alias is legal gets swallowed as an alias — this is
why `CROSS` and `USING` are reserved keywords rather than identifiers matched by value.

## Pull requests

- `gofmt -l .` prints nothing, `go vet ./...` is clean, `go test ./...` passes.
- One logical change per PR.
- Explain *why* in the description. The code says what.
- New behaviour comes with tests; new SQL comes with a `features` entry.
