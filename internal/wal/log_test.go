package wal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/types"
)

// openAt opens a log in a fresh directory and replays it, collecting the
// transactions it held.
func openAt(t *testing.T, dir string) (*Log, [][]Record) {
	t.Helper()
	l, err := Open(dir, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var got [][]Record
	if err := l.Replay(func(recs []Record) error {
		got = append(got, recs)
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l, got
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()

	want := [][]Record{
		{DDLRecord("create table t (a int, b text)")},
		{
			InsertRecord("public.t", 1, []types.Value{types.Int(1), types.Text("one")}),
			InsertRecord("public.t", 2, []types.Value{types.Int(2), types.Null()}),
		},
		{
			DeleteRecord("public.t", 1),
			InsertRecord("public.t", 3, []types.Value{types.Int(7), types.Text("seven")}),
		},
		{MissingRecord("public.t", 2, types.Timestamp(1_700_000_000_000_000))},
	}

	l, got := openAt(t, dir)
	if len(got) != 0 {
		t.Fatalf("a fresh log replayed %d transactions", len(got))
	}
	for _, tx := range want {
		if err := l.Write(tx); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, got = openAt(t, dir)
	assertLog(t, got, want)
}

func assertLog(t *testing.T, got, want [][]Record) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("replayed %d transactions, want %d", len(got), len(want))
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("transaction %d has %d records, want %d", i, len(got[i]), len(want[i]))
		}
		for j, w := range want[i] {
			g := got[i][j]
			if g.Kind != w.Kind || g.SQL != w.SQL || g.Table != w.Table ||
				g.Row != w.Row || g.Column != w.Column || len(g.Vals) != len(w.Vals) {
				t.Fatalf("transaction %d record %d = %+v, want %+v", i, j, g, w)
			}
			for k := range w.Vals {
				if g.Vals[k].Kind() != w.Vals[k].Kind() || !types.Equal(g.Vals[k], w.Vals[k]) {
					t.Fatalf("transaction %d record %d value %d = %v, want %v",
						i, j, k, g.Vals[k], w.Vals[k])
				}
			}
		}
	}
}

// TestTruncatedTailIsDiscarded is the crash-injection test. A commit is not one
// atomic write at the level the operating system offers, so a crash can leave
// any prefix of it on disk. Recovery must yield the transactions before it and
// nothing else -- half a transaction is worse than none, because the rows it
// did write would violate the constraints the rest of it satisfied.
//
// It also has to leave the file appendable. Keeping the partial tail would push
// the next transaction past unreadable bytes, and every commit after this
// restart would be lost at the one after it.
func TestTruncatedTailIsDiscarded(t *testing.T) {
	full := [][]Record{
		{DDLRecord("create table t (a int)")},
		{InsertRecord("public.t", 1, []types.Value{types.Int(1)})},
		{InsertRecord("public.t", 2, []types.Value{types.Int(2)})},
	}

	// Build a complete log once, then replay every prefix of its bytes.
	dir := t.TempDir()
	l, _ := openAt(t, dir)
	for _, tx := range full {
		if err := l.Write(tx); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	complete, err := os.ReadFile(filepath.Join(dir, Name))
	if err != nil {
		t.Fatal(err)
	}

	for n := len(magic); n <= len(complete); n++ {
		t.Run("", func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, Name), complete[:n], 0o644); err != nil {
				t.Fatal(err)
			}

			l, got := openAt(t, dir)
			if len(got) > len(full) {
				t.Fatalf("replayed %d transactions from a %d-byte prefix", len(got), n)
			}
			assertLog(t, got, full[:len(got)])

			// The repaired log must accept a further transaction and read it
			// back alongside the ones that survived.
			next := []Record{InsertRecord("public.t", 3, []types.Value{types.Int(3)})}
			if err := l.Write(next); err != nil {
				t.Fatalf("Write after recovery: %v", err)
			}
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			_, again := openAt(t, dir)
			assertLog(t, again, append(append([][]Record{}, got...), next))
		})
	}
}

// TestCorruptFrameStopsReplay pins that a checksum failure in the middle of the
// log ends recovery there rather than skipping the frame. The length that says
// where the next frame begins is part of what the checksum covers, so nothing
// after a bad frame can be trusted to be a frame at all.
func TestCorruptFrameStopsReplay(t *testing.T) {
	dir := t.TempDir()
	l, _ := openAt(t, dir)
	for i := range 3 {
		if err := l.Write([]Record{InsertRecord("public.t", uint64(i), []types.Value{types.Int(int64(i))})}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	l.Close()

	path := filepath.Join(dir, Name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a bit inside the second frame's payload.
	first, _, err := nextFrame(data[len(magic):])
	if err != nil || first == nil {
		t.Fatalf("nextFrame on a good log: %v", err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, got := openAt(t, dir)
	if len(got) != 2 {
		t.Fatalf("replayed %d transactions, want the 2 before the damaged one", len(got))
	}
}

// TestCheckpointReplacesHistory pins that a checkpoint leaves a log describing
// the state rather than the history, and that it stays appendable afterwards.
func TestCheckpointReplacesHistory(t *testing.T) {
	dir := t.TempDir()
	l, _ := openAt(t, dir)
	for i := range 5 {
		if err := l.Write([]Record{InsertRecord("public.t", uint64(i), []types.Value{types.Int(int64(i))})}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	state := []Record{
		DDLRecord("create table t (a int)"),
		InsertRecord("public.t", 4, []types.Value{types.Int(4)}),
	}
	if err := l.Checkpoint(state); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	after := []Record{InsertRecord("public.t", 5, []types.Value{types.Int(5)})}
	if err := l.Write(after); err != nil {
		t.Fatalf("Write after checkpoint: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, got := openAt(t, dir)
	assertLog(t, got, [][]Record{state, after})

	if _, err := os.Stat(filepath.Join(dir, Name+".tmp")); !os.IsNotExist(err) {
		t.Error("the checkpoint's temporary file was left behind")
	}
}

// TestEmptyCheckpointLeavesAReadableLog covers the state of a database whose
// tables have all been dropped: there is nothing to write, and the result must
// still be a log rather than an empty file.
func TestEmptyCheckpointLeavesAReadableLog(t *testing.T) {
	dir := t.TempDir()
	l, _ := openAt(t, dir)
	if err := l.Write([]Record{DDLRecord("create table t (a int)")}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := l.Checkpoint(nil); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	l.Close()

	_, got := openAt(t, dir)
	if len(got) != 0 {
		t.Fatalf("replayed %d transactions after an empty checkpoint", len(got))
	}
}

// TestForeignDirectoryIsRefused pins that lightsql will not adopt a file it did
// not write. Treating an unrecognised file as an empty log would mean the first
// commit overwrote whatever was there.
func TestForeignDirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Name), []byte("SQLite format 3\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, false); err == nil {
		t.Error("Open accepted a file it did not write")
	}
}

// TestWriteBeforeReplayIsRefused guards the ordering that makes recovery a
// repair: appending before the partial tail has been removed would put a good
// frame after a bad one, where the next restart could never reach it.
func TestWriteBeforeReplayIsRefused(t *testing.T) {
	l, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Write([]Record{DDLRecord("create table t (a int)")}); err == nil {
		t.Error("Write succeeded before Replay")
	}
}

// FuzzNextFrame asserts that arbitrary bytes never panic. Recovery reads
// whatever is on disk after a crash, and the frame header says how many records
// and how many bytes to expect -- so the decoder is handed lengths it must
// treat as claims rather than as facts.
func FuzzNextFrame(f *testing.F) {
	l := &Log{}
	f.Add(l.encode([]Record{DDLRecord("create table t (a int)")}))
	f.Add(l.encode([]Record{InsertRecord("public.t", 1, []types.Value{types.Int(1), types.Text("x")})}))
	f.Add(l.encode([]Record{DeleteRecord("public.t", 1), MissingRecord("public.t", 0, types.Null())}))

	f.Fuzz(func(t *testing.T, data []byte) {
		recs, rest, err := nextFrame(data)
		if err != nil || recs == nil {
			return
		}
		if len(rest) >= len(data) {
			t.Fatalf("decoding a frame from %d bytes consumed nothing", len(data))
		}
	})
}

// TestRecordSize guards the field order. A record is copied into the pending
// slice of every transaction that writes a row, and grouping the two small
// fields is what keeps the padding out; a natural-looking order costs a whole
// extra word per row and would not otherwise be noticed.
func TestRecordSize(t *testing.T) {
	const want = 72
	if got := unsafe.Sizeof(Record{}); got != want {
		t.Errorf("unsafe.Sizeof(Record{}) = %d, want %d", got, want)
	}
}

// TestSecondOpenIsRefused pins the lock. Two processes with one directory open
// do not merely race: a checkpoint in one renames a new file over the log, and
// the other goes on writing to an inode that no longer has a name, losing
// everything it commits from then on with no error on either side.
//
// This happened. A server was running while a second program opened the same
// directory to look at it, and that program's ordinary close -- which
// checkpoints -- orphaned the server's descriptor. The server carried on
// committing into a file nobody could see.
func TestSecondOpenIsRefused(t *testing.T) {
	dir := t.TempDir()

	first, _ := openAt(t, dir)

	// flock is held per open file description, so a second open from this same
	// process is refused exactly as another process would be.
	if _, err := Open(dir, false); err == nil {
		t.Fatal("a second open of the same directory succeeded")
	} else {
		if !strings.Contains(err.Error(), "another process") {
			t.Errorf("error %q does not say the directory is already open", err)
		}
		var coded interface{ SQLState() string }
		if errors.As(err, &coded) && coded.SQLState() != pgerr.ObjectInUse {
			t.Errorf("SQLSTATE = %s, want %s", coded.SQLState(), pgerr.ObjectInUse)
		}
	}

	// Releasing it makes the directory available again.
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second, err := Open(dir, false)
	if err != nil {
		t.Fatalf("opening after the first was closed: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestLockSurvivesACheckpoint is the regression test for the bug itself. A
// checkpoint renames a new file over the log, so a lock taken on the log would
// end up on the unlinked inode and the next process would lock the replacement
// -- both believing they held the directory. The lock is on a file of its own
// for exactly this reason.
func TestLockSurvivesACheckpoint(t *testing.T) {
	dir := t.TempDir()

	held, _ := openAt(t, dir)
	if err := held.Write([]Record{DDLRecord("create table t (a int)")}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := held.Checkpoint([]Record{DDLRecord("create table t (a int)")}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// The log is now a different file than the one opened. The lock is not.
	if _, err := Open(dir, false); err == nil {
		t.Fatal("the directory became available to a second opener after a checkpoint")
	}

	// And the holder can still write, into the file that still has a name.
	if err := held.Write([]Record{InsertRecord("public.t", 1, []types.Value{types.Int(1)})}); err != nil {
		t.Fatalf("writing after a checkpoint: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, got := openAt(t, dir)
	if len(got) != 2 {
		t.Errorf("replayed %d transactions, want the checkpoint and the write after it", len(got))
	}
}

// TestCheckpointKeepsThePreviousLog is the fix for a one-way door.
//
// Compacting is what makes a dropped table unrecoverable, and closing is what
// compacts -- so shutting a database down cleanly destroyed history that a
// crash would have left intact. That is backwards, and it cost someone a
// hundred and seventy-six rows: they stopped the process to preserve the
// evidence, and stopping it was what removed the evidence.
func TestCheckpointKeepsThePreviousLog(t *testing.T) {
	dir := t.TempDir()

	l, _ := openAt(t, dir)
	history := [][]Record{
		{DDLRecord("create table t (a int)")},
		{InsertRecord("public.t", 1, []types.Value{types.Int(1)})},
		{InsertRecord("public.t", 2, []types.Value{types.Int(2)})},
	}
	for _, tx := range history {
		if err := l.Write(tx); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// A checkpoint describing a state in which the table is gone, which is what
	// closing after a DROP TABLE produces.
	if err := l.Checkpoint([]Record{DDLRecord("create table other (a int)")}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The live log is the compacted one.
	_, now := openAt(t, dir)
	if len(now) != 1 {
		t.Fatalf("the live log holds %d transactions, want the checkpoint", len(now))
	}

	// The history is still there under the other name, and is a whole log: a
	// copy of it into a directory of its own opens and replays.
	old := t.TempDir()
	data, err := os.ReadFile(filepath.Join(dir, PreviousName))
	if err != nil {
		t.Fatalf("reading the previous log: %v", err)
	}
	if err := os.WriteFile(filepath.Join(old, Name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, recovered := openAt(t, old)
	assertLog(t, recovered, history)
}

// TestIdleCloseKeepsTheGeneration pins what makes one generation enough to be
// useful. Opening a database and closing it again without writing anything must
// leave both files alone -- otherwise the first thing anyone does after
// noticing a mistake, which is open the database to look at it, replaces the
// history they came for with the state they already have.
func TestIdleCloseKeepsTheGeneration(t *testing.T) {
	dir := t.TempDir()

	l, _ := openAt(t, dir)
	if err := l.Write([]Record{DDLRecord("create table t (a int)")}); err != nil {
		t.Fatal(err)
	}
	if err := l.Checkpoint([]Record{DDLRecord("create table gone (a int)")}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	kept, err := os.ReadFile(filepath.Join(dir, PreviousName))
	if err != nil {
		t.Fatalf("the previous log was not kept: %v", err)
	}

	// Open, read nothing, close. Twice, since a mistake is rarely noticed on
	// the first look.
	for range 2 {
		again, _ := openAt(t, dir)
		if err := again.Checkpoint([]Record{DDLRecord("create table gone (a int)")}); err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}
		if err := again.Close(); err != nil {
			t.Fatal(err)
		}
	}

	now, err := os.ReadFile(filepath.Join(dir, PreviousName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kept, now) {
		t.Error("looking at the database replaced the history it was kept for")
	}
}

// TestCheckpointKeepsOnlyOneGeneration pins the bound. Keeping every generation
// would grow without limit, and the window that matters is the one between a
// mistake and someone noticing it.
//
// The generation rotates only when something was actually written, so a run of
// checkpoints does not walk the history off the end: two writes give two
// rotations, and the kept copy is the one the second replaced.
func TestCheckpointKeepsOnlyOneGeneration(t *testing.T) {
	dir := t.TempDir()

	l, _ := openAt(t, dir)
	for _, table := range []string{"first", "second"} {
		if err := l.Write([]Record{DDLRecord("create table " + table + " (a int)")}); err != nil {
			t.Fatal(err)
		}
		if err := l.Checkpoint([]Record{DDLRecord("checkpoint after " + table)}); err != nil {
			t.Fatalf("checkpoint after %s: %v", table, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	old := t.TempDir()
	data, err := os.ReadFile(filepath.Join(dir, PreviousName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, Name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, recovered := openAt(t, old)
	// What the second checkpoint replaced: the first checkpoint's own content
	// followed by the write that came after it.
	if len(recovered) != 2 || recovered[0][0].SQL != "checkpoint after first" {
		t.Errorf("the kept generation is %v, want the one the last checkpoint replaced", recovered)
	}
}

// TestPreviousLogDoesNotDisturbRecovery pins that the extra file is inert: it
// is not read, not truncated, and not mistaken for the log.
func TestPreviousLogDoesNotDisturbRecovery(t *testing.T) {
	dir := t.TempDir()

	l, _ := openAt(t, dir)
	if err := l.Write([]Record{DDLRecord("create table t (a int)")}); err != nil {
		t.Fatal(err)
	}
	if err := l.Checkpoint([]Record{DDLRecord("create table t (a int)")}); err != nil {
		t.Fatal(err)
	}
	next := []Record{InsertRecord("public.t", 1, []types.Value{types.Int(1)})}
	if err := l.Write(next); err != nil {
		t.Fatalf("writing after a checkpoint: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, got := openAt(t, dir)
	assertLog(t, got, [][]Record{{DDLRecord("create table t (a int)")}, next})

	before, err := os.Stat(filepath.Join(dir, PreviousName))
	if err != nil {
		t.Fatal(err)
	}
	// Reopening must not touch it. The directory has to be released first --
	// the lock refuses a second opener, which is #62 working as intended.
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, _ := openAt(t, dir)
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(dir, PreviousName))
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() {
		t.Errorf("the previous log changed size on reopen: %d then %d", before.Size(), after.Size())
	}
}
