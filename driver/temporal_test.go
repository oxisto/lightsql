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
		// A timestamp-shaped string casts to date by truncation, as in
		// PostgreSQL -- the date is the part of it that was asked for.
		{"date truncates a timestamp", `SELECT '2024-01-02 12:30:00'::DATE`, "2024-01-02"},
		{"date truncates a zoned timestamp", `SELECT '2024-01-02T12:30:00Z'::DATE`, "2024-01-02"},
		// Before the epoch the instant is negative, so truncation has to floor
		// rather than divide toward zero or the date lands a day late.
		{"date before the epoch", `SELECT '1969-06-15 12:00:00'::DATE`, "1969-06-15"},
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

// TestDatetimeValueFunctions covers the keyword forms of "what time is it".
//
// Each one names a different part of the transaction's start, and they share a
// payload field, so the risk is not that one of them fails but that one of them
// answers with another's units. CURRENT_DATE reading microseconds as a day
// count is a date three billion years hence, which no assertion on "did it
// return a time" would notice.
func TestDatetimeValueFunctions(t *testing.T) {
	db := open(t)

	begin := time.Now().UTC().Add(-time.Second)

	tests := []struct {
		name  string
		query string
		check func(t *testing.T, got time.Time)
	}{
		{
			name:  "CURRENT_TIMESTAMP is now",
			query: `SELECT CURRENT_TIMESTAMP`,
			check: func(t *testing.T, got time.Time) { assertRecent(t, got, begin) },
		},
		{
			name:  "LOCALTIMESTAMP is now",
			query: `SELECT LOCALTIMESTAMP`,
			check: func(t *testing.T, got time.Time) { assertRecent(t, got, begin) },
		},
		{
			name:  "CURRENT_DATE is today at midnight",
			query: `SELECT CURRENT_DATE`,
			check: func(t *testing.T, got time.Time) {
				if h, m, s := got.Clock(); h != 0 || m != 0 || s != 0 {
					t.Errorf("CURRENT_DATE = %s, want a midnight", got.Format(time.RFC3339Nano))
				}
				wantY, wantM, wantD := time.Now().UTC().Date()
				if y, mo, d := got.Date(); y != wantY || mo != wantM || d != wantD {
					t.Errorf("CURRENT_DATE = %s, want %04d-%02d-%02d",
						got.Format(time.RFC3339), wantY, wantM, wantD)
				}
			},
		},
		{
			name:  "CURRENT_TIME is a time of day",
			query: `SELECT CURRENT_TIME`,
			check: assertTimeOfDay,
		},
		{
			name:  "LOCALTIME is a time of day",
			query: `SELECT LOCALTIME`,
			check: assertTimeOfDay,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got time.Time
			if err := db.QueryRow(tt.query).Scan(&got); err != nil {
				t.Fatalf("%s: %v", tt.query, err)
			}
			tt.check(t, got.UTC())
		})
	}
}

// assertRecent checks that a timestamp is between the moment the test started
// and a moment after it, which is as tight as an assertion about "now" can be.
func assertRecent(t *testing.T, got, begin time.Time) {
	t.Helper()
	end := time.Now().UTC().Add(time.Second)
	if got.Before(begin) || got.After(end) {
		t.Errorf("got %s, want something between %s and %s",
			got.Format(time.RFC3339Nano), begin.Format(time.RFC3339Nano),
			end.Format(time.RFC3339Nano))
	}
}

// assertTimeOfDay checks that a time carries no date, which is how lightsql
// stores one: microseconds since midnight, rendered on the epoch day.
func assertTimeOfDay(t *testing.T, got time.Time) {
	t.Helper()
	if y, m, d := got.Date(); y != 1970 || m != time.January || d != 1 {
		t.Errorf("got %s, want a time of day on the epoch date",
			got.Format(time.RFC3339Nano))
	}
	wantHour := time.Now().UTC().Hour()
	if h := got.Hour(); h != wantHour {
		t.Errorf("got hour %d, want %d", h, wantHour)
	}
}

// TestDatetimeValueFunctionsAgreeWithinATransaction pins the property that
// makes these usable in a migration: every statement in one transaction sees
// the same "now", so two rows inserted together cannot disagree about when they
// were written. PostgreSQL guarantees exactly this.
func TestDatetimeValueFunctionsAgreeWithinATransaction(t *testing.T) {
	db := open(t)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	var first, viaNow, second time.Time
	if err := tx.QueryRow(`SELECT CURRENT_TIMESTAMP`).Scan(&first); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := tx.QueryRow(`SELECT now()`).Scan(&viaNow); err != nil {
		t.Fatalf("now(): %v", err)
	}
	if err := tx.QueryRow(`SELECT CURRENT_TIMESTAMP`).Scan(&second); err != nil {
		t.Fatalf("second: %v", err)
	}

	if !first.Equal(second) {
		t.Errorf("CURRENT_TIMESTAMP moved within a transaction: %s then %s",
			first.Format(time.RFC3339Nano), second.Format(time.RFC3339Nano))
	}
	// The two spellings must also agree with each other, or a schema mixing
	// them would record two different times for one transaction.
	if !first.Equal(viaNow) {
		t.Errorf("CURRENT_TIMESTAMP is %s but now() is %s",
			first.Format(time.RFC3339Nano), viaNow.Format(time.RFC3339Nano))
	}
}

// TestCurrentTimestampAsDefault is the form the issue that prompted this was
// actually about: a column that stamps itself, written the way a schema ported
// from another dialect writes it.
func TestCurrentTimestampAsDefault(t *testing.T) {
	db := open(t)

	if _, err := db.Exec(`CREATE TABLE sessions (
		id         TEXT PRIMARY KEY,
		created    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		created_on DATE      NOT NULL DEFAULT CURRENT_DATE
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id) VALUES ('a')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	var created, createdOn time.Time
	err := db.QueryRow(`SELECT created, created_on FROM sessions`).Scan(&created, &createdOn)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	assertRecent(t, created.UTC(), time.Now().UTC().Add(-time.Minute))
	if h, m, s := createdOn.UTC().Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("a DATE default kept a time of day: %s", createdOn.Format(time.RFC3339Nano))
	}

	// The comparison a session table actually runs.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sessions WHERE created <= CURRENT_TIMESTAMP`).Scan(&n); err != nil {
		t.Fatalf("comparison: %v", err)
	}
	if n != 1 {
		t.Errorf("comparing against CURRENT_TIMESTAMP matched %d rows, want 1", n)
	}
}
