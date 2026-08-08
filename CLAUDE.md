# lightsql — working notes

A small embeddable SQL engine speaking the PostgreSQL dialect, for unit tests and
small demo deployments. Written from scratch as a successor to `ramsql`, whose
design problems are the reason several rules below are absolute.

## Commands

```sh
go test ./...                              # everything
go test ./internal/parser -run TestExpr     # one area
go test ./internal/features -update         # regenerate the README matrix
go test ./internal/scanner -fuzz FuzzScan -fuzztime 30s
go test ./... -race
gofmt -l .                                  # must print nothing
```

## Architecture

```
SQL text
  └─ internal/scanner   → []token.Token   keyword map, escapes resolved, byte offsets
  └─ internal/parser    → ast.Stmt        recursive descent + Pratt expressions
  └─ internal/binder    → plan.Node       names → ordinals, types inferred, * expanded
  └─ internal/plan      → plan.Node       predicate pushdown, index selection
  └─ internal/exec      → operators       streaming iterators, expressions compiled to closures
  └─ internal/storage                     MVCC heap + indexes + transaction manager
```

Support packages: `internal/token` (kinds, positions), `internal/ast` (typed nodes and
the S-expression printer), `internal/types` (`Value`, `Bool3`, comparison),
`internal/catalog`, `internal/wal`, `internal/pgerr` (SQLSTATE), `internal/builtin`,
`internal/features` (the compatibility registry).

`driver/` is a thin `database/sql` adapter and holds no logic. The root package is the
public API.

## Invariants

These are load-bearing. Breaking one is a design change, not a detail.

1. **No runtime dependencies** outside the standard library and `golang.org/x/*`.
   Enforced by `TestNoRuntimeDependencies`. Test-only dependencies go in `compat/`,
   which is a separate module.

2. **Every token and node carries a position**, and every error carries a SQLSTATE code
   plus a position. Never strip whitespace before parsing, never rebuild a statement
   from lexemes. Construct errors with `pgerr`, never `fmt.Errorf`, on any path that can
   reach a caller.

3. **The AST is typed and immutable.** One node type per construct. Never add a generic
   node with a slice of untyped children, never navigate by child index, and never
   mutate a node while walking it — a prepared statement is planned once and executed
   many times.

4. **A column value is `types.Value`, never `any`.** No `reflect` on a per-row path, no
   `fmt.Sprintf` to compare or hash a value. `NULL` is `KindNull`, never a nil
   interface.

5. **Three-valued logic is not optional.** SQL comparisons return `types.Bool3`. Only
   `True` passes a filter. `Compare` (a total order, NULLs last, used by sorts and
   indexes) is deliberately different from `Eq` (SQL semantics, NULL-poisoning) — do not
   collapse them.

6. **Equal values must hash equally.** `Compare` promotes across numeric kinds, so
   `Value.Hash` mixes in a hash *class*, not the raw kind. Adding a kind that compares
   across kinds means updating `hashClass`.

7. **Nothing below the binder compares names as strings.** Column references become
   ordinals in the binder. String comparison per row is a bug, not a slow path.

8. **Context is honoured inside operator `Next()`**, not merely checked at statement
   entry. A running query must be cancellable.

9. **No package-level mutable state, and no global logger configuration.** A library
   does not set the process log level. Configuration arrives through the connector.

## Adding a SQL feature

Use the `sql-feature` skill (`.claude/skills/sql-feature`), which walks the pipeline in
order. The short version:

1. `internal/token` — new keyword? Add the constant *and* the `keywords` map entry.
   Ask whether it must be reserved: an unreserved word that appears where an alias is
   legal will be swallowed as an alias. That is why `CROSS` and `USING` are keywords.
2. `internal/scanner` — only for new lexical syntax. Add a table test case.
3. `internal/ast` — a new typed node with a `Pos()`, plus a `print.go` case.
4. `internal/parser` — the production, plus a case in `TestExprPrecedence` or
   `TestParseStatements` asserting the whole tree, and an error case with its position.
5. `internal/binder`, `internal/plan`, `internal/exec` — resolution and execution.
6. `internal/features` — add or update the entry, with a probe statement. Then
   `go test ./internal/features -update` to regenerate the README.

Step 6 is not optional: the matrix is generated and tested, so a stale entry fails CI.

## Commit and PR style

Go project convention: `package: lower-case imperative summary`, at most 72 characters,
no trailing period.

```
exec: fix non-terminating scan on SELECT without FROM
scanner, parser: carry positions through quoted identifiers
all: gofmt after the Go 1.27 comment change
docs: settle on Go-style commit messages
```

Use the package base name, not the `internal/` path. Several packages are listed
most-significant-first; tree-wide changes use `all:`; repository docs and tooling use
`docs:`, `ci:` or `build:`. Do **not** use `feat:` / `fix:` prefixes — the convention
here names *where* a change lands, which is the useful axis in a staged pipeline.

The body explains why, wrapped at 72 columns. PR titles follow the same format.
Dependabot's `build(deps):` subjects are left alone.

## Conventions

- Comments explain *why*, especially where the code deliberately differs from an obvious
  simpler approach. Several such comments name the ramsql failure mode they prevent;
  keep that style, it is the point.
- Doc comments must not contain a doubled single quote — `gofmt` rewrites it into a
  typographic quote. Write "a doubled single quote" instead.
- Table-driven tests with a `name` field. Assert on whole trees via `ast.Sprint`, not on
  individual fields.
- Error messages are lower case, without a trailing period, mirroring PostgreSQL's
  wording so parity tests can compare them.
