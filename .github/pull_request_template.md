<!--
Title this pull request like a commit subject:  package: lower-case summary
for example                                     parser: add UNION and EXCEPT

The repository squash-merges, so the title becomes the commit subject on main
and is checked by CI. See CONTRIBUTING.md#commit-messages.
-->

## Why

<!-- The problem this solves, or the issue it closes. The diff already says what
     changed; this should say why it needed to. -->

## Approach

<!-- Only if a reader would wonder: what was chosen, and what was rejected. -->

## Verification

<!-- Anything beyond `go test ./...`: a query run by hand, a benchmark, a parity
     check against a real PostgreSQL. Delete if the test suite covers it. -->

---

- [ ] `gofmt -l .` is empty, `go vet ./...` is clean, `go test ./...` passes
- [ ] New behaviour has tests; a fixed bug has the test that would have caught it
- [ ] New SQL has an `internal/features` entry with a probe, and the README was
      regenerated with `go test ./internal/features -update`
