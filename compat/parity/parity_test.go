//go:build parity

// Package parity runs the same SQL against lightsql and a real PostgreSQL and
// compares what comes back.
//
// It is the highest-value correctness tool available to a project whose whole
// claim is "speaks the PostgreSQL dialect". Everything else here checks lightsql
// against someone's reading of the documentation; this checks it against the
// thing itself. The first rule it settled was PostgreSQL's division scale for
// numeric, which had been reproduced from the source and could not otherwise be
// confirmed -- and where reasoning about it by hand had produced a wrong answer.
//
// It is behind a build tag and needs a database, so it does not run by default:
//
//	docker run -d --name pg -e POSTGRES_PASSWORD=parity -e POSTGRES_DB=parity \
//	    -e PGDATA=/pgdata --tmpfs /pgdata -p 55432:5432 postgres:17-alpine
//	LIGHTSQL_PARITY_DSN='postgres://postgres:parity@localhost:55432/parity?sslmode=disable' \
//	    go test -tags parity ./parity/...
package parity

import (
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	lightsqldriver "github.com/oxisto/lightsql/driver"
)

// pair is the two databases a case runs against.
type pair struct {
	pg *sql.DB
	ls *sql.DB
}

func open(t *testing.T) *pair {
	t.Helper()

	dsn := os.Getenv("LIGHTSQL_PARITY_DSN")
	if dsn == "" {
		t.Skip("set LIGHTSQL_PARITY_DSN to a PostgreSQL database to run the parity suite")
	}

	pg, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening PostgreSQL: %v", err)
	}
	if err := pg.Ping(); err != nil {
		t.Fatalf("reaching PostgreSQL at %s: %v", dsn, err)
	}
	// One connection, because the schema below is set on the session rather
	// than on the database. A pool would hand later statements a connection
	// that never saw the SET.
	pg.SetMaxOpenConns(1)

	// A schema per test, dropped afterwards, so cases cannot see each other's
	// tables and a failure leaves nothing behind for the next run to trip over.
	schema := "parity_" + sanitise(t.Name())
	exec(t, pg, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	exec(t, pg, "CREATE SCHEMA "+schema)
	exec(t, pg, "SET search_path TO "+schema)

	name := "parity-" + sanitise(t.Name())
	ls, err := sql.Open("lightsql", name)
	if err != nil {
		t.Fatalf("opening lightsql: %v", err)
	}

	t.Cleanup(func() {
		exec(t, pg, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		pg.Close()
		ls.Close()
		lightsqldriver.Drop(name)
	})
	return &pair{pg: pg, ls: ls}
}

// sanitise turns a test name into something usable as an identifier.
func sanitise(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func exec(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("%s\n  %v", stmt, err)
	}
}

// setup runs schema and data statements against both, requiring both to accept
// them. A case whose fixture one of them rejects is a finding in itself.
func (p *pair) setup(t *testing.T, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := p.pg.Exec(s); err != nil {
			t.Fatalf("PostgreSQL rejected the fixture:\n  %s\n  %v", s, err)
		}
		if _, err := p.ls.Exec(s); err != nil {
			t.Fatalf("lightsql rejected a fixture PostgreSQL accepted:\n  %s\n  %v", s, err)
		}
	}
}

// result is what a query produced: either rows or a SQLSTATE.
type result struct {
	rows  []string
	state string
	err   error
}

func (r result) String() string {
	if r.err != nil {
		return fmt.Sprintf("error %s: %v", r.state, r.err)
	}
	if len(r.rows) == 0 {
		return "(no rows)"
	}
	return strings.Join(r.rows, "\n    ")
}

// run executes a query and renders the outcome in a form the two databases can
// be compared on.
//
// Every column is scanned as text, which is the only representation both sides
// can be asked for without the comparison becoming an argument about Go types.
// It is also what makes a difference visible rather than papered over: a numeric
// carrying a different scale shows up as different text, which is exactly the
// kind of divergence worth catching.
func run(db *sql.DB, query string) result {
	rows, err := db.Query(query)
	if err != nil {
		return result{state: sqlState(err), err: err}
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return result{state: sqlState(err), err: err}
	}

	var out []string
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return result{state: sqlState(err), err: err}
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = "NULL"
			if c.Valid {
				parts[i] = c.String
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		return result{state: sqlState(err), err: err}
	}
	return result{rows: out}
}

// sqlState pulls the five-character code out of an error from either driver.
//
// Both satisfy the same interface: it is the one lib/pq and pgx settled on, and
// the reason lightsql's errors implement it. That is what lets this compare
// failures and not only successes -- an engine that fails differently is only
// slightly better than one that does not fail at all.
func sqlState(err error) string {
	for err != nil {
		if s, ok := err.(interface{ SQLState() string }); ok {
			return s.SQLState()
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return "?????"
}

// check runs one query against both and reports any difference.
func (p *pair) check(t *testing.T, query string) {
	t.Helper()

	want := run(p.pg, query)
	got := run(p.ls, query)

	switch {
	case want.err != nil && got.err != nil:
		if want.state != got.state {
			t.Errorf("%s\n  both refused it, with different codes:\n"+
				"    postgres %s: %v\n    lightsql %s: %v",
				query, want.state, want.err, got.state, got.err)
		}
	case want.err != nil:
		t.Errorf("%s\n  postgres refused it (%s: %v)\n  lightsql accepted it:\n    %s",
			query, want.state, want.err, got)
	case got.err != nil:
		t.Errorf("%s\n  postgres returned:\n    %s\n  lightsql refused it (%s: %v)",
			query, want, got.state, got.err)
	default:
		// A query without ORDER BY has no defined row order, so comparing the
		// sequences would report a difference where SQL guarantees nothing.
		// Both sides are sorted in that case, which is what sqllogictest calls
		// rowsort and for the same reason. A query that does order its rows is
		// compared in order, because then the order is part of the answer.
		if !strings.Contains(strings.ToUpper(query), "ORDER BY") {
			want.rows = sorted(want.rows)
			got.rows = sorted(got.rows)
		}
		if strings.Join(want.rows, "\n") != strings.Join(got.rows, "\n") {
			t.Errorf("%s\n  postgres:\n    %s\n  lightsql:\n    %s", query, want, got)
		}
	}
}

func sorted(rows []string) []string {
	out := slices.Clone(rows)
	slices.Sort(out)
	return out
}

// checkAll runs a list of queries, each as its own subtest so one difference
// does not hide the rest.
func (p *pair) checkAll(t *testing.T, queries ...string) {
	t.Helper()
	for _, q := range queries {
		t.Run(short(q), func(t *testing.T) { p.check(t, q) })
	}
}

// checkKnown records a difference that is known and accepted, asserting that it
// is still there.
//
// Deleting such a case would be the easy thing and the wrong one: the
// divergence would then be invisible, and so would the day it went away. This
// fails if the two ever agree, which is the signal to promote the case to
// check and delete the excuse.
func (p *pair) checkKnown(t *testing.T, reason, query string) {
	t.Helper()
	t.Run("known: "+short(query), func(t *testing.T) {
		want, got := run(p.pg, query), run(p.ls, query)
		same := want.err == nil && got.err == nil &&
			strings.Join(want.rows, "\n") == strings.Join(got.rows, "\n")
		if same {
			t.Errorf("%s\n  this is recorded as a known difference (%s)\n"+
				"  but the two now agree -- move it to check and delete the note",
				query, reason)
			return
		}
		t.Logf("known difference (%s)\n  postgres:\n    %s\n  lightsql:\n    %s",
			reason, want, got)
	})
}

func short(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if len(q) > 60 {
		q = q[:60]
	}
	return q
}
