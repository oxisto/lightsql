package catalog

import (
	"testing"

	"github.com/oxisto/lightsql/internal/types"
)

// newTestTable returns a two-column table holding the given integer rows.
func newTestTable(t *testing.T, values ...int64) *Table {
	t.Helper()

	intType, err := ResolveType("bigint", nil)
	if err != nil {
		t.Fatalf("ResolveType: %v", err)
	}
	tbl := &Table{
		Name:    "t",
		Columns: []Column{{Name: "a", Type: intType}, {Name: "b", Type: intType}},
	}
	if _, err := New().CreateTable(tbl, false); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	for _, v := range values {
		if err := tbl.Insert([]types.Value{types.Int(v), types.Int(v * 10)}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	return tbl
}

// TestMutateDoesNotDisturbSnapshots is the test the doc comment on Mutate points
// at. Mutate swaps in a new outer slice, but it hands the callback the stored
// row itself, so the no-write-through rule lives in the callback. A caller that
// edited the row in place would corrupt a reader that already holds a snapshot,
// and nothing but this test would notice.
func TestMutateDoesNotDisturbSnapshots(t *testing.T) {
	tbl := newTestTable(t, 1, 2, 3)

	// A reader takes its snapshot before the write starts, as a running scan
	// would.
	snapshot := tbl.Rows()
	if len(snapshot) != 3 {
		t.Fatalf("snapshot has %d rows, want 3", len(snapshot))
	}

	// A well-behaved caller returns a fresh slice rather than editing in place.
	err := tbl.Mutate(func(row []types.Value) ([]types.Value, error) {
		next := make([]types.Value, len(row))
		copy(next, row)
		next[1] = types.Int(999)
		return next, nil
	})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	// The snapshot must still show the values it was taken with.
	for i, row := range snapshot {
		if got, want := row[1].AsInt(), int64((i+1)*10); got != want {
			t.Errorf("snapshot row %d column b = %d, want %d; the update wrote through",
				i, got, want)
		}
	}
	// And the table must show the new ones.
	for i, row := range tbl.Rows() {
		if got := row[1].AsInt(); got != 999 {
			t.Errorf("table row %d column b = %d, want 999", i, got)
		}
	}
}

// TestMutateIsAtomic pins that a callback failing partway leaves the table
// exactly as it was, rather than applying the rows it had already reached.
func TestMutateIsAtomic(t *testing.T) {
	tbl := newTestTable(t, 1, 2, 3)

	wantErr := errSentinel("boom")
	err := tbl.Mutate(func(row []types.Value) ([]types.Value, error) {
		if row[0].AsInt() == 2 {
			return nil, wantErr
		}
		next := make([]types.Value, len(row))
		copy(next, row)
		next[1] = types.Int(999)
		return next, nil
	})
	if err != wantErr {
		t.Fatalf("Mutate returned %v, want the callback's error", err)
	}

	rows := tbl.Rows()
	if len(rows) != 3 {
		t.Fatalf("table has %d rows, want 3 after a failed mutation", len(rows))
	}
	for i, row := range rows {
		if got, want := row[1].AsInt(), int64((i+1)*10); got != want {
			t.Errorf("row %d column b = %d, want %d; a failed mutation was partly applied",
				i, got, want)
		}
	}
}

// TestMutateDeletesAndPreservesOrder checks that returning nil removes a row and
// that the survivors keep their relative order.
func TestMutateDeletesAndPreservesOrder(t *testing.T) {
	tbl := newTestTable(t, 1, 2, 3, 4)

	err := tbl.Mutate(func(row []types.Value) ([]types.Value, error) {
		if row[0].AsInt()%2 == 0 {
			return nil, nil // delete the even ones
		}
		return row, nil
	})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	var got []int64
	for _, row := range tbl.Rows() {
		got = append(got, row[0].AsInt())
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("remaining rows = %v, want [1 3]", got)
	}
}

// TestMutateEnforcesNotNull checks that a replacement row is validated, so a
// constraint cannot be bypassed by going through an update.
func TestMutateEnforcesNotNull(t *testing.T) {
	tbl := newTestTable(t, 1)
	tbl.Columns[1].NotNull = true

	err := tbl.Mutate(func(row []types.Value) ([]types.Value, error) {
		next := make([]types.Value, len(row))
		copy(next, row)
		next[1] = types.Null()
		return next, nil
	})
	if err == nil {
		t.Fatal("Mutate accepted a NULL in a NOT NULL column")
	}
	if got := tbl.Rows()[0][1].AsInt(); got != 10 {
		t.Errorf("row was modified despite the error: column b = %d, want 10", got)
	}
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
