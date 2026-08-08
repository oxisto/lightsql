---
name: sql-feature
description: Add or extend SQL support in lightsql — walks the token → scanner → AST → parser → binder → plan → exec pipeline in order and updates the generated compatibility matrix. Use when adding a SQL statement, clause, operator, function or type, when a query fails with "syntax error at or near", or when asked to update the README compatibility table.
---

# Adding a SQL feature to lightsql

lightsql is a pipeline. A feature that touches SQL almost always touches several stages,
and skipping one produces a confusing failure much later — an unreserved keyword eaten
as an alias, a node the printer renders as `UNKNOWN`, a matrix row that lies.

Work in pipeline order. At each stage, add the test **before** moving on.

## 0. Decide the scope

Find the feature in `internal/features/features.go`. If it is not there, add it. Its
`Parse` and `Exec` statuses tell you which stages already exist and which you are about
to build. If it is marked `No`, check with the user before implementing — that status
means someone decided it was out of scope.

## 1. `internal/token` — only if new keywords are involved

Add the constant inside the `keywordBegin`/`keywordEnd` block **and** the entry in the
`keywords` map. The `String()` name is derived automatically, so do not add one.

**Then ask: must this keyword be reserved?** A word that is not a keyword arrives as
`token.Ident`, and anywhere an alias is legal — after a select item, after a table name
— the alias rule will consume it. `CROSS` and `USING` are keywords precisely because
`FROM t CROSS JOIN u` otherwise parses `cross` as an alias of `t`.

If the word is genuinely unreserved (`nulls`, `first`, `zone`, `row`), leave it as an
identifier and match it with `p.atWord("nulls")` / `p.expectWord("zone")`.

## 2. `internal/scanner` — only for new lexical syntax

New operator, literal form or comment syntax. Two rules hold:

- Quote characters never reach the parser; escapes are resolved here.
- A sign is never folded into a numeric literal.

Add a case to the table in `scanner_test.go`, and an error case with its expected byte
position. Then fuzz it:

```sh
go test ./internal/scanner -fuzz FuzzScan -fuzztime 30s
```

## 3. `internal/ast` — the node

One typed node per construct, with a `Pos()` method and an `exprNode()`/`stmtNode()`/
`tableExprNode()` marker. Do not extend an existing node with a mode flag to make it
mean two things, and do not add a generic node with untyped children.

Add the matching case to `print.go`. If `ast.Sprint` renders `(UNKNOWN ...)` you missed
it. Multi-word operator names are hyphenated by `opTag` so a head stays one atom.

## 4. `internal/parser` — the production

Statements and clauses go in `parser.go` as recursive descent. Operators go in `expr.go`
via the precedence table — add the token to `infixOps` with the right binding power
rather than special-casing it at a call site.

Tests, in `parser_test.go`:

- Operators: a case in `TestExprPrecedence` asserting the **whole tree**, including at
  least one case that shows it nesting correctly against a neighbouring precedence
  level. `a * b + c` is the canonical example — a parser that stops after one operator
  still "parses" it.
- Statements: a case in `TestParseStatements` or `TestParseCreateTable`.
- A case in `TestParseErrors` with the exact byte position.

Then fuzz: `go test ./internal/parser -fuzz FuzzParse -fuzztime 30s`.

## 5. `internal/binder`, `internal/plan`, `internal/exec`

Resolution, planning, execution. Invariants that apply here:

- Column references become **ordinals** in the binder. No string comparison per row.
- Values are `types.Value`. Comparisons return `types.Bool3`, and only `True` passes a
  filter.
- `types.Compare` is the total order for sorting and indexing; `types.Eq` is SQL
  equality. Pick deliberately.
- Check `ctx.Err()` inside operator `Next()`.
- Errors come from `pgerr` with a real SQLSTATE — `42703` for an unknown column, `42883`
  for an unknown function, `42804` for a type mismatch.

## 6. `internal/features` — update the matrix, always

Set `Parse` and `Exec` to what is now true, write a `Note` if support is partial (a
`Partial` status without a `Note` fails the test), and make sure `SQL` is a probe that
demonstrates the feature.

The probe is checked: a feature claiming `Parse: Yes` whose probe does not parse fails,
and so does one claiming `Planned` whose probe *does* parse. That second direction
matters — it catches the matrix understating what works.

Regenerate the README:

```sh
go test ./internal/features -update
```

## 7. Verify

```sh
gofmt -l .          # must print nothing
go vet ./...
go test ./...
```

If a real PostgreSQL is available, check the dialect matches rather than assuming:

```sh
LIGHTSQL_PARITY_DSN='postgres://...' go test -tags parity ./...
```

## Common mistakes

- Adding a keyword to the constant block but not the `keywords` map — it silently stays
  an identifier.
- Making a word a keyword when it should not be, which breaks `SELECT a AS <word>`.
- Asserting on one AST field instead of the whole tree, which hides a dropped operand.
- Testing an error's message but not its position.
- Using `fmt.Errorf` on a path that reaches a caller, losing the SQLSTATE.
- Leaving `internal/features` alone, which fails CI on the generated README.
