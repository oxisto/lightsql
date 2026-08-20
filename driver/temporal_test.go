package driver_test

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTemporalLiterals covers writing a date, time or timestamp as text, which
// is the only way to write one in SQL: there is no date literal syntax, so
// without this a temporal column could not be given a value from a statement at
// all, only from a time.Time argument.
func TestTemporalLiterals(t *testing.T) {
	db := open(t)

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{"date", `SELECT '2024-01-02'::DATE`, "2024-01-02"},
		{"timestamp with a space", `SELECT '2024-01-02 12:30:00'::TIMESTAMP`, "2024-01-02 12:30:00"},
		// Both separators are in the wild: SQL writes the space, RFC 3339 and
		// JSON write the T, and one migration file routinely holds both.
		{"timestamp with a T", `SELECT '2024-01-02T12:30:00'::TIMESTAMP`, "2024-01-02 12:30:00"},
		{"fractional seconds", `SELECT '2024-01-02T12:30:00.123456'::TIMESTAMP`, "2024-01-02 12:30:00.123456"},
		{"date promotes to timestamp", `SELECT '2024-01-02'::TIMESTAMP`, "2024-01-02 00:00:00"},
		// A time is microseconds since midnight, so it scans as a time.Time on
		// the epoch date. The date part carries no meaning.
		{"time", `SELECT '12:30:00'::TIME`, "1970-01-01 12:30:00"},
		{"time without seconds", `SELECT '12:30'::TIME`, "1970-01-01 12:30:00"},
		// An offset on a timestamptz is honoured and normalised to UTC.
		{"timestamptz offset", `SELECT '2024-01-02T12:30:00+02:00'::TIMESTAMPTZ`, "2024-01-02 10:30:00"},
		{"timestamptz Z", `SELECT '2024-01-02T12:30:00Z'::TIMESTAMPTZ`, "2024-01-02 12:30:00"},
		// "without time zone" means the offset is dropped, not applied. Shifting
		// it would make one literal mean two instants depending on the column it
		// landed in.
		{"timestamp ignores an offset", `SELECT '2024-01-02T12:30:00+02:00'::TIMESTAMP`, "2024-01-02 12:30:00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rowsOf(t, db, tt.query)
			if len(got) != 1 || !strings.HasPrefix(got[0], tt.want) {
				t.Errorf("%s\n got: %v\nwant it to start with: %q", tt.query, got, tt.want)
			}
		})
	}
}

func TestTemporalLiteralErrors(t *testing.T) {
	db := open(t)

	tests := []struct {
		name  string
		query string
	}{
		{"not a date at all", `SELECT 'nonsense'::DATE`},
		{"out of range", `SELECT '2024-13-45'::DATE`},
		{"empty", `SELECT ''::TIMESTAMP`},
		// Trailing rubbish must not be ignored: a partial match that silently
		// succeeded would store a different instant than was written.
		{"trailing text", `SELECT '2024-01-02 12:30:00 and more'::TIMESTAMP`},
		// PostgreSQL's non-ISO styles are deliberately not accepted, because
		// 01/02/2024 has no reading that is right in both conventions.
		{"slash form is not accepted", `SELECT '01/02/2024'::DATE`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := queryErr(db, tt.query)
			if err == nil {
				t.Fatalf("%s: expected an error", tt.query)
			}
			if !strings.Contains(err.Error(), "invalid input syntax") {
				t.Errorf("got %v, want an invalid-input error", err)
			}
		})
	}
}

// TestTemporalInStatements exercises the paths a migration and a query actually
// take: an implicit conversion into a column, a DEFAULT, a CHECK, a comparison
// and a sort. Each resolves the literal somewhere different in the binder.
func TestTemporalInStatements(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE ev (id INT, at TIMESTAMP DEFAULT '2024-01-01 00:00:00', d DATE)`,
		`INSERT INTO ev (id, at, d) VALUES (1, '2024-03-01 12:00:00', '2024-03-01')`,
		`INSERT INTO ev (id, d) VALUES (2, '2024-04-01')`,
		`INSERT INTO ev (id, at, d) VALUES (3, '2024-02-01T08:00:00Z', '2024-02-01')`,
	)

	t.Run("default applies", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT id FROM ev WHERE at = '2024-01-01 00:00:00'`)
		if want := []string{"2"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// A bare literal on the other side of a comparison takes the column's type,
	// which is what makes `WHERE at > '2024-01-01'` work without a cast.
	t.Run("comparison against a literal", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT id FROM ev WHERE d > '2024-02-15' ORDER BY id`)
		if want := []string{"1", "2"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("sorts chronologically", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT id FROM ev ORDER BY at`)
		if want := []string{"2", "3", "1"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("between two literals", func(t *testing.T) {
		got := rowsOf(t, db, `SELECT id FROM ev WHERE at BETWEEN '2024-01-15 00:00:00' AND '2024-03-15 00:00:00' ORDER BY id`)
		if want := []string{"1", "3"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("check constraint over a literal", func(t *testing.T) {
		mustExecAll(t, db, `CREATE TABLE chk (at TIMESTAMP CHECK (at > '2020-01-01 00:00:00'))`)
		if _, err := db.Exec(`INSERT INTO chk VALUES ('2024-01-01 00:00:00')`); err != nil {
			t.Fatalf("valid row rejected: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO chk VALUES ('2019-01-01 00:00:00')`); err == nil {
			t.Error("expected the CHECK to reject a row before 2020")
		}
	})

	// A literal and a time.Time argument must land on the same instant, or the
	// same row would be found by one and missed by the other.
	t.Run("literal agrees with a time.Time argument", func(t *testing.T) {
		want := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ev WHERE at = ?`, want).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("argument matched %d rows, want 1", n)
		}
	})
}

// TestTemporalCrossKind covers comparing and assigning across the temporal
// kinds, which is what `expires_at <= now()` needs when the column has no time
// zone.
//
// These need a real conversion rather than permission to compare: a date counts
// days while a timestamp counts microseconds, so comparing the payloads
// directly would answer confidently and wrongly.
func TestTemporalCrossKind(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE s (id INT, at TIMESTAMP, d DATE, tz TIMESTAMPTZ)`,
		`INSERT INTO s VALUES (1, '2024-06-01 12:00:00', '2024-06-01', '2024-06-01T12:00:00Z')`,
	)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"timestamp against timestamptz", `SELECT id FROM s WHERE at = tz`, []string{"1"}},
		{"date against timestamp", `SELECT id FROM s WHERE d < at`, []string{"1"}},
		// A date is midnight, so it is not the same instant as noon that day.
		{"date is midnight", `SELECT id FROM s WHERE d = at`, nil},
		{"timestamp against now()", `SELECT id FROM s WHERE at < now()`, []string{"1"}},
		{"timestamptz against now()", `SELECT id FROM s WHERE tz < now()`, []string{"1"}},
		{"date against now()", `SELECT id FROM s WHERE d < now()`, []string{"1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rowsOf(t, db, tt.query); !slices.Equal(got, tt.want) {
				t.Errorf("%s\n got: %v\nwant: %v", tt.query, got, tt.want)
			}
		})
	}

	// A timestamptz assigned into a column with no zone, which is what a
	// DEFAULT now() on a TIMESTAMP column does.
	t.Run("assignment across kinds", func(t *testing.T) {
		mustExecAll(t, db,
			`CREATE TABLE a (id INT, at TIMESTAMP DEFAULT now())`,
			`INSERT INTO a (id) VALUES (1)`)
		if got := rowsOf(t, db, `SELECT count(*) FROM a WHERE at IS NOT NULL`); !slices.Equal(got, []string{"1"}) {
			t.Errorf("got %v, want [1]", got)
		}
	})
}
