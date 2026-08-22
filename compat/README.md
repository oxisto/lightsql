# compat

Test suites that need dependencies.

This is a **separate module** on purpose. The root `go.mod` is guarded by
`TestNoRuntimeDependencies`, so a PostgreSQL driver or an ORM listed there would
be inherited by every project that imports lightsql. Keeping them here lets these
suites use real drivers while the thing they are testing stays dependency-free.

Nothing here runs as part of `go test ./...` from the repository root: it is a
different module, and the parity suite is behind a build tag as well.

## Parity

`parity/` runs the same SQL against lightsql and a real PostgreSQL and compares
what comes back — result sets *and* SQLSTATEs.

It is the highest-value correctness tool this project has. Everything else
checks lightsql against someone's reading of the documentation; this checks it
against the thing itself.

```sh
docker run -d --name pg \
    -e POSTGRES_PASSWORD=parity -e POSTGRES_DB=parity \
    -e PGDATA=/pgdata --tmpfs /pgdata \
    -p 55432:5432 postgres:17-alpine

LIGHTSQL_PARITY_DSN='postgres://postgres:parity@localhost:55432/parity?sslmode=disable' \
    go test -tags parity ./parity/...
```

Without the environment variable the suite skips, so it is safe to leave in a
`go test ./...` over this module.

### How a case is compared

Every column is scanned as text, which is the only representation both sides can
be asked for without the comparison becoming an argument about Go types. It also
makes a difference visible rather than papering over it — a numeric carrying a
different scale shows up as different text.

A query **without `ORDER BY`** has its rows sorted on both sides before
comparing, since SQL guarantees no order there. One **with** `ORDER BY` is
compared in order, because then the order is part of the answer. This is what
sqllogictest calls rowsort, for the same reason.

Failures are compared too. An engine that never refuses anything would otherwise
look perfect.

### Known differences

`checkKnown` records a difference that is understood and accepted, and **fails if
the two ever agree**. Deleting such a case would be easier and worse: the
divergence would become invisible, and so would the day it went away.

There is one today — PostgreSQL raises a numeric to a numeric power exactly,
while lightsql computes it in `float64`, so `2 ^ 0.5` differs in the last digit.

### What it found

Every one of these was a real defect, found the first time this ran:

- Unary minus on a decimal went through `float64`, so `-1.7` briefly stopped
  being exact. Arithmetic hid it by converting back through text; a cast to
  integer did not, and failed.
- `ORDER BY 2` when there is no second column reported `42601`; PostgreSQL
  reports `42P10`.
- A malformed date reported `22P02`; PostgreSQL reports `22007`.
- `CAST(1 AS BOOLEAN)` was refused. PostgreSQL accepts it.
- `UPDATE t SET c = DEFAULT` was a syntax error.

It also confirmed the thing that could not be confirmed any other way: the scale
PostgreSQL picks for numeric division, reproduced from its source, is right in
every case tested — including one where reasoning about it by hand had given the
wrong answer.
