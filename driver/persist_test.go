package driver_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	lightsqldriver "github.com/oxisto/lightsql/driver"
)

// onDisk returns a database in a directory of its own, together with a function
// that closes it the way an application exiting would.
//
// The two are separate because most of what is worth testing here happens
// across a close: the interesting question is never what a running database
// says, it is what the next one says after it is gone.
func onDisk(t *testing.T, dir string) (db *sql.DB, shut func()) {
	t.Helper()

	dsn := "file:" + dir
	db, err := sql.Open("lightsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dsn, err)
	}
	// Touching the database is what actually opens it; sql.Open only parses.
	if err := db.Ping(); err != nil {
		t.Fatalf("opening %q: %v", dsn, err)
	}

	closed := false
	shut = func() {
		if closed {
			return
		}
		closed = true
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
		if !lightsqldriver.Drop(dsn) {
			t.Errorf("Drop(%q) found no instance", dsn)
		}
	}
	t.Cleanup(shut)
	return db, shut
}

// mustExec runs a statement and fails the test if it does not.
func mustExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// scanStrings runs a query and returns each row as a single string, so a whole
// table can be compared in one assertion.
func scanStrings(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if !s.Valid {
			out = append(out, "<null>")
			continue
		}
		out = append(out, s.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func assertRows(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d rows %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDataSurvivesARestart is the acceptance test for persistence: everything
// else in this file is a way for it to be wrong.
func TestDataSurvivesARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo.db")

	db, shut := onDisk(t, dir)
	mustExec(t, db, `CREATE TABLE users (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL, joined TIMESTAMP DEFAULT now())`)
	mustExec(t, db, `INSERT INTO users (name) VALUES ('Alice'), ('Bob'), ('Carol')`)
	mustExec(t, db, `UPDATE users SET name = 'Roberta' WHERE name = 'Bob'`)
	mustExec(t, db, `DELETE FROM users WHERE name = 'Carol'`)
	before := scanStrings(t, db, `SELECT name FROM users ORDER BY id`)
	shut()

	again, _ := onDisk(t, dir)
	assertRows(t, scanStrings(t, again, `SELECT name FROM users ORDER BY id`), before)
	assertRows(t, before, []string{"Alice", "Roberta"})
}

// TestRecoveryWithoutACleanClose is the case that matters most, because it is
// the one that happens by accident. The directory is copied while the database
// is still open, so nothing has been checkpointed and the copy is exactly what
// a crash would have left behind.
func TestRecoveryWithoutACleanClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo.db")

	db, _ := onDisk(t, dir)
	mustExec(t, db, `CREATE TABLE t (a INT PRIMARY KEY, b TEXT)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'one'), (2, 'two')`)
	mustExec(t, db, `UPDATE t SET b = 'zwei' WHERE a = 2`)

	crashed := copyDir(t, dir)
	after, _ := onDisk(t, crashed)
	assertRows(t, scanStrings(t, after, `SELECT b FROM t ORDER BY a`), []string{"one", "zwei"})
}

// TestRolledBackWorkIsNotOnDisk is the disk half of what makes rollback free.
// In memory nothing was overwritten; on disk nothing was written.
func TestRolledBackWorkIsNotOnDisk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo.db")

	db, shut := onDisk(t, dir)
	mustExec(t, db, `CREATE TABLE t (a INT)`)
	mustExec(t, db, `INSERT INTO t VALUES (1)`)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO t VALUES (2)`); err != nil {
		t.Fatalf("INSERT in a transaction: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	shut()

	again, _ := onDisk(t, dir)
	assertRows(t, scanStrings(t, again, `SELECT a FROM t ORDER BY a`), []string{"1"})
}

// TestSequencesDoNotRestart pins that a serial column keeps going after a
// restart. Restarting it would hand a new row the id of one that is still
// there, and the primary key would refuse the insert -- which is at least loud,
// but only because there is a key. Without one it would quietly duplicate.
func TestSequencesDoNotRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo.db")

	db, shut := onDisk(t, dir)
	mustExec(t, db, `CREATE TABLE t (id SERIAL PRIMARY KEY, name TEXT)`)
	mustExec(t, db, `INSERT INTO t (name) VALUES ('a'), ('b'), ('c')`)
	shut()

	again, _ := onDisk(t, dir)
	mustExec(t, again, `INSERT INTO t (name) VALUES ('d')`)
	assertRows(t, scanStrings(t, again, `SELECT id FROM t ORDER BY id`),
		[]string{"1", "2", "3", "4"})
}

// TestAddedColumnKeepsItsMissingValue covers a row that is narrower than its
// table. ADD COLUMN does not rewrite the rows already stored, so the ones
// written before it read the value the column records for them -- and that has
// to come back across a restart, along with the rows that do carry the column.
func TestAddedColumnKeepsItsMissingValue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo.db")

	db, shut := onDisk(t, dir)
	mustExec(t, db, `CREATE TABLE t (a INT)`)
	mustExec(t, db, `INSERT INTO t VALUES (1)`)
	mustExec(t, db, `ALTER TABLE t ADD COLUMN grade INT DEFAULT 42`)
	mustExec(t, db, `INSERT INTO t VALUES (2, 7)`)
	shut()

	again, _ := onDisk(t, dir)
	assertRows(t, scanStrings(t, again, `SELECT grade FROM t ORDER BY a`),
		[]string{"42", "7"})
}

// TestSchemaChangesSurvive covers the statements that make a name mean
// something different, which is what a log records by name has to get right.
func TestSchemaChangesSurvive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo.db")

	db, shut := onDisk(t, dir)
	mustExec(t, db, `CREATE TABLE scratch (a INT)`)
	mustExec(t, db, `INSERT INTO scratch VALUES (1)`)
	mustExec(t, db, `DROP TABLE scratch`)

	mustExec(t, db, `CREATE TABLE old (a INT, b TEXT)`)
	mustExec(t, db, `INSERT INTO old VALUES (1, 'one')`)
	mustExec(t, db, `ALTER TABLE old RENAME TO renamed`)
	mustExec(t, db, `ALTER TABLE renamed RENAME COLUMN b TO label`)
	mustExec(t, db, `CREATE UNIQUE INDEX renamed_a ON renamed (a)`)
	shut()

	again, _ := onDisk(t, dir)
	assertRows(t, scanStrings(t, again, `SELECT label FROM renamed`), []string{"one"})
	if _, err := again.Exec(`INSERT INTO scratch VALUES (2)`); err == nil {
		t.Error("a dropped table came back after a restart")
	}
	if _, err := again.Exec(`INSERT INTO renamed VALUES (1, 'again')`); err == nil {
		t.Error("a unique index was not enforced after a restart")
	}
}

// TestRenameAfterWritingInOneTransaction is the case that decided how a record
// names its table. The rename reaches the log first, because DDL is written as
// it runs while rows wait for the commit -- so by the time recovery reaches the
// rows, the name they were written under is gone.
func TestRenameAfterWritingInOneTransaction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo.db")

	db, shut := onDisk(t, dir)
	mustExec(t, db, `CREATE TABLE before (a INT)`)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO before VALUES (1)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := tx.Exec(`ALTER TABLE before RENAME TO after`); err != nil {
		t.Fatalf("RENAME: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	shut()

	again, _ := onDisk(t, dir)
	assertRows(t, scanStrings(t, again, `SELECT a FROM after`), []string{"1"})
}

// TestDropAfterWritingInOneTransaction is the other half of that case: the rows
// went with the table in memory, so replaying them into a table that no longer
// exists would fail recovery over work that was already discarded.
func TestDropAfterWritingInOneTransaction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo.db")

	db, shut := onDisk(t, dir)
	mustExec(t, db, `CREATE TABLE t (a INT)`)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO t VALUES (1)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := tx.Exec(`DROP TABLE t`); err != nil {
		t.Fatalf("DROP: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	shut()

	again, _ := onDisk(t, dir)
	if _, err := again.Exec(`INSERT INTO t VALUES (2)`); err == nil {
		t.Error("a table dropped in the same transaction that wrote to it came back")
	}
}

// TestConstraintsSurvive pins that what comes back is a schema rather than a
// pile of rows. The catalog keeps DEFAULT expressions and CHECK predicates as
// syntax, and replaying the statement is what puts them back.
func TestConstraintsSurvive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo.db")

	db, shut := onDisk(t, dir)
	mustExec(t, db, `CREATE TABLE parent (id INT PRIMARY KEY)`)
	mustExec(t, db, `CREATE TABLE child (
		id     INT PRIMARY KEY,
		parent INT NOT NULL REFERENCES parent(id) ON DELETE CASCADE,
		grade  INT CHECK (grade BETWEEN 1 AND 6),
		note   TEXT DEFAULT 'none'
	)`)
	mustExec(t, db, `INSERT INTO parent VALUES (1)`)
	mustExec(t, db, `INSERT INTO child (id, parent, grade) VALUES (1, 1, 3)`)
	shut()

	again, _ := onDisk(t, dir)
	assertRows(t, scanStrings(t, again, `SELECT note FROM child`), []string{"none"})

	if _, err := again.Exec(`INSERT INTO child (id, parent, grade) VALUES (2, 99, 3)`); err == nil {
		t.Error("a foreign key was not enforced after a restart")
	}
	if _, err := again.Exec(`INSERT INTO child (id, parent, grade) VALUES (2, 1, 9)`); err == nil {
		t.Error("a CHECK constraint was not enforced after a restart")
	}
	mustExec(t, again, `DELETE FROM parent WHERE id = 1`)
	assertRows(t, scanStrings(t, again, `SELECT id FROM child`), nil)
}

// TestCheckpointCompacts pins that a database which is written to repeatedly
// does not carry every version it ever held. Without it the log grows for as
// long as the database is used, and a restart replays the whole history rather
// than the rows that survived it.
func TestCheckpointCompacts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo.db")

	db, shut := onDisk(t, dir)
	mustExec(t, db, `CREATE TABLE counter (id INT PRIMARY KEY, n INT)`)
	mustExec(t, db, `INSERT INTO counter VALUES (1, 0)`)
	for range 500 {
		mustExec(t, db, `UPDATE counter SET n = n + 1 WHERE id = 1`)
	}
	grown := logSize(t, dir)
	shut()

	compacted := logSize(t, dir)
	if compacted >= grown {
		t.Errorf("the log is %d bytes after a checkpoint and %d before; it should have shrunk",
			compacted, grown)
	}

	again, _ := onDisk(t, dir)
	assertRows(t, scanStrings(t, again, `SELECT n FROM counter`), []string{"500"})
}

// TestFsyncCanBeTurnedOff covers the option being accepted and the database
// still working with it set. Whether the flush actually stops happening is not
// observable from here -- that would need to watch the syscall -- so this pins
// the half that a typo in the option name would break.
func TestFsyncCanBeTurnedOff(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo.db")
	dsn := "file:" + dir + "?fsync=off"

	db, err := sql.Open("lightsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { lightsqldriver.Drop(dsn) })

	mustExec(t, db, `CREATE TABLE t (a INT)`)
	mustExec(t, db, `INSERT INTO t VALUES (1)`)
	assertRows(t, scanStrings(t, db, `SELECT a FROM t`), []string{"1"})

	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}
}

// TestUnknownOptionIsRefused pins that a setting lightsql does not understand
// is an error. Accepting and discarding one is how a database ends up running
// without the durability its caller asked for.
func TestUnknownOptionIsRefused(t *testing.T) {
	db, err := sql.Open("lightsql", "file:"+t.TempDir()+"?journal_mode=wal")
	if err == nil {
		err = db.Ping()
	}
	if err == nil {
		t.Error("an unknown data source option was accepted")
	}
}

// TestMemoryAndFileAreDifferentDatabases pins that the scheme is part of an
// instance's identity, so a directory called mytest and an in-memory instance
// of that name do not collide in the registry.
func TestMemoryAndFileAreDifferentDatabases(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")

	file, _ := onDisk(t, dir)
	mustExec(t, file, `CREATE TABLE t (a INT)`)

	mem, err := sql.Open("lightsql", dir)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { lightsqldriver.Drop(dir) })
	if _, err := mem.Exec(`INSERT INTO t VALUES (1)`); err == nil {
		t.Error("an in-memory instance reached a file-backed one of the same name")
	}
}

// TestConcurrentCommitsAllReachTheLog runs the shape a database/sql pool
// produces: one engine, many connections, all committing at once. Each
// transaction is one frame, so the log has to serialise them without
// interleaving two half-written frames -- and every acknowledged commit has to
// be there afterwards, since that is what acknowledging it promised.
func TestConcurrentCommitsAllReachTheLog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo.db")

	db, shut := onDisk(t, dir)
	mustExec(t, db, `CREATE TABLE t (id INT PRIMARY KEY, who TEXT)`)

	const writers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				_, err := db.Exec(`INSERT INTO t VALUES ($1, $2)`, w*each+i, strconv.Itoa(w))
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent INSERT: %v", err)
	}
	shut()

	again, _ := onDisk(t, dir)
	assertRows(t, scanStrings(t, again, `SELECT count(*) FROM t`),
		[]string{strconv.Itoa(writers * each)})
}

// logSize reports how large the write-ahead log is.
func logSize(t *testing.T, dir string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return info.Size()
}

// copyDir copies a database directory, standing in for the state a crash would
// leave: whatever had reached the disk, with nothing checkpointed.
func copyDir(t *testing.T, dir string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "crashed.db")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}
