package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// session runs the shell over the given input and returns what it wrote.
//
// It calls run rather than building and executing a binary, so a failure points
// at a line of Go rather than at a subprocess.
func session(t *testing.T, args []string, stdin string) (out, errs string, err error) {
	t.Helper()
	var stdout, stderr strings.Builder
	err = run(args, strings.NewReader(stdin), &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

// dbDir returns a path for a database that does not exist yet, since creating
// it is part of what is being tested.
func dbDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "demo.db")
}

// TestPersistsAcrossInvocations is the reason the tool exists: being able to
// open a directory afterwards and see what is really in it. Two separate runs
// of the shell, with nothing shared but the path.
func TestPersistsAcrossInvocations(t *testing.T) {
	dir := dbDir(t)

	if _, errs, err := session(t, []string{"-c", `
		CREATE TABLE users (id INT PRIMARY KEY, name TEXT NOT NULL);
		INSERT INTO users VALUES (1, 'Alice'), (2, 'Bob');
	`, dir}, ""); err != nil {
		t.Fatalf("first run: %v\n%s", err, errs)
	}

	out, errs, err := session(t, []string{"-c", "SELECT name FROM users ORDER BY id", dir}, "")
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, errs)
	}
	for _, want := range []string{"Alice", "Bob", "(2 rows)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// TestStatementSplitting covers the two cases a shell gets wrong if it looks
// for semicolons rather than scanning: one inside a string literal, and a
// statement spread over several lines.
func TestStatementSplitting(t *testing.T) {
	dir := dbDir(t)
	if _, _, err := session(t, []string{"-c",
		`CREATE TABLE t (id INT PRIMARY KEY, note TEXT)`, dir}, ""); err != nil {
		t.Fatal(err)
	}

	out, errs, err := session(t, []string{dir}, strings.Join([]string{
		"INSERT INTO t VALUES",
		"  (1, 'a;b'),",
		"  (2, 'plain');",
		"SELECT note FROM t WHERE id = 1;",
	}, "\n"))
	if err != nil {
		t.Fatalf("%v\n%s", err, errs)
	}
	if !strings.Contains(out, "a;b") {
		t.Errorf("the literal containing a semicolon did not survive:\n%s", out)
	}
	if !strings.Contains(out, "INSERT 2 rows") {
		t.Errorf("the multi-line insert did not report two rows:\n%s", out)
	}
}

// TestTrailingStatementNeedsNoSemicolon pins that `-c 'SELECT 1'` works without
// punctuation, which is how anyone typing one at a shell prompt writes it.
func TestTrailingStatementNeedsNoSemicolon(t *testing.T) {
	out, errs, err := session(t, []string{"-c", "SELECT 1 AS n"}, "")
	if err != nil {
		t.Fatalf("%v\n%s", err, errs)
	}
	if !strings.Contains(out, "(1 row)") {
		t.Errorf("output:\n%s", out)
	}
}

// TestFormats covers the two machine-readable forms. The row count must not
// appear in either: a consumer parsing CSV would have to know to drop a
// trailing line that is not CSV.
func TestFormats(t *testing.T) {
	dir := dbDir(t)
	if _, _, err := session(t, []string{"-c", `
		CREATE TABLE t (id INT PRIMARY KEY, name TEXT, score NUMERIC(5,2), ok BOOLEAN);
		INSERT INTO t VALUES (1, 'Alice', 9.5, true), (2, NULL, NULL, false);
	`, dir}, ""); err != nil {
		t.Fatal(err)
	}

	csv, _, err := session(t, []string{"-format", "csv", "-c", "SELECT * FROM t ORDER BY id", dir}, "")
	if err != nil {
		t.Fatal(err)
	}
	// 9.50 rather than 9.5: the column is NUMERIC(5,2), so the value is stored
	// to the scale it was declared with, as it would be in PostgreSQL.
	wantCSV := "id,name,score,ok\n1,Alice,9.50,true\n2,NULL,NULL,false\n"
	if csv != wantCSV {
		t.Errorf("csv =\n%q\nwant\n%q", csv, wantCSV)
	}

	// JSON keeps the types, which is the whole reason to choose it over CSV: a
	// number stays a number and NULL becomes null rather than the word.
	js, _, err := session(t, []string{"-format", "json", "-c", "SELECT * FROM t ORDER BY id", dir}, "")
	if err != nil {
		t.Fatal(err)
	}
	// The decimal goes out as a JSON number with its digits intact, not as a
	// string and not rounded through a float.
	for _, want := range []string{`"id": 1`, `"score": 9.50`, `"ok": true`, `"name": null`} {
		if !strings.Contains(js, want) {
			t.Errorf("json does not contain %s:\n%s", want, js)
		}
	}
	if strings.Contains(js, "rows)") {
		t.Errorf("json output carries a row count:\n%s", js)
	}
}

// TestDotCommands covers the commands working from a pipe, not only from a
// terminal, since a script that lists tables is a reasonable thing to write.
func TestDotCommands(t *testing.T) {
	dir := dbDir(t)
	if _, _, err := session(t, []string{"-c",
		`CREATE TABLE users (id INT PRIMARY KEY, note TEXT DEFAULT 'none')`, dir}, ""); err != nil {
		t.Fatal(err)
	}

	out, errs, err := session(t, []string{dir}, ".tables\n.schema users\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, errs)
	}
	for _, want := range []string{"users", "'none'", "PRIMARY KEY"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}

	// The system schemas are the shell's own plumbing, and listing them among
	// the user's tables would bury them.
	if strings.Contains(out, "pg_catalog") {
		t.Errorf(".tables listed the system schemas:\n%s", out)
	}
}

// TestTransactionCommands pins that the shell supplies BEGIN and ROLLBACK,
// since the engine has no such statements -- a transaction is taken through the
// driver, which is what lets it honour an isolation level.
func TestTransactionCommands(t *testing.T) {
	dir := dbDir(t)
	if _, _, err := session(t, []string{"-c", `
		CREATE TABLE t (id INT PRIMARY KEY);
		INSERT INTO t VALUES (1);
	`, dir}, ""); err != nil {
		t.Fatal(err)
	}

	out, errs, err := session(t, []string{dir}, strings.Join([]string{
		".begin",
		"INSERT INTO t VALUES (2);",
		"SELECT count(*) FROM t;",
		".rollback",
		"SELECT count(*) FROM t;",
	}, "\n")+"\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, errs)
	}
	// Two counts, in order: inside the transaction, then after discarding it.
	first := strings.Index(out, "2")
	if first < 0 || !strings.Contains(out[first:], "1") {
		t.Errorf("expected a count of 2 inside the transaction and 1 after the rollback:\n%s", out)
	}
}

// TestTransactionVerbsAreRedirected pins the hint. A syntax error on BEGIN is
// technically right and completely unhelpful: the reader has written valid SQL
// that this engine takes through the driver instead.
func TestTransactionVerbsAreRedirected(t *testing.T) {
	dir := dbDir(t)
	_, _, err := session(t, []string{"-c", "CREATE TABLE t (a INT); BEGIN;", dir}, "")
	if err == nil {
		t.Fatal("BEGIN was accepted as SQL")
	}
	if !strings.Contains(err.Error(), ".begin") {
		t.Errorf("error does not point at the shell command:\n%v", err)
	}
}

// TestScriptStopsAtTheFirstError pins that a script does not carry on past a
// failure. The statements after one usually depend on it, and running them
// produces a cascade whose first line was the only one worth reading.
func TestScriptStopsAtTheFirstError(t *testing.T) {
	dir := dbDir(t)
	_, _, err := session(t, []string{dir}, strings.Join([]string{
		"CREATE TABLE t (a INT);",
		"SELECT * FROM nope;",
		"INSERT INTO t VALUES (1);",
	}, "\n"))
	if err == nil {
		t.Fatal("the script did not fail")
	}

	out, _, qerr := session(t, []string{"-c", "SELECT count(*) FROM t", dir}, "")
	if qerr != nil {
		t.Fatal(qerr)
	}
	if !strings.Contains(out, "0") {
		t.Errorf("statements after the failure ran anyway:\n%s", out)
	}
}

// TestFileArgumentIsRefused pins the mistake worth catching early: a database
// is a directory, and pointing the shell at a file should say so rather than
// failing from somewhere inside the log.
func TestFileArgumentIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session(t, []string{"-c", "SELECT 1", path}, ""); err == nil {
		t.Error("a plain file was accepted as a database")
	}
}

// TestInMemoryNeedsNoPath covers the quickest thing the tool does: try a query
// against nothing at all.
func TestInMemoryNeedsNoPath(t *testing.T) {
	out, errs, err := session(t, nil, "CREATE TABLE t (a INT);\nINSERT INTO t VALUES (1);\nSELECT a FROM t;\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, errs)
	}
	if !strings.Contains(out, "(1 row)") {
		t.Errorf("output:\n%s", out)
	}
}
