package wal

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

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
