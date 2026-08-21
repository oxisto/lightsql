package driver_test

import (
	"database/sql"
	"slices"
	"testing"
	"time"
)

// portSchema is money-gopher's schema in the shape a PostgreSQL port gives it:
// migration 1 with the SQLite type names translated, and migration 2 expressed
// as ADD COLUMN rather than SQLite's create-copy-drop-rename rebuild.
//
// It is written out here rather than read from the other repository so that
// this test pins lightsql's behaviour and cannot start failing because someone
// edited a file next door.
var portSchema = []string{
	`CREATE TABLE users (
		id TEXT PRIMARY KEY,
		issuer TEXT NOT NULL,
		subject TEXT NOT NULL,
		display_name TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT now(),
		UNIQUE (issuer, subject))`,
	`CREATE TABLE persons (
		id TEXT PRIMARY KEY,
		display_name TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT now())`,
	`CREATE TABLE user_person_access (
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		person_id TEXT NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
		PRIMARY KEY (user_id, person_id))`,
	`CREATE TABLE cash_accounts (
		id TEXT PRIMARY KEY,
		display_name TEXT NOT NULL,
		currency TEXT NOT NULL,
		iban TEXT,
		created_at TIMESTAMP NOT NULL DEFAULT now())`,
	`ALTER TABLE cash_accounts ADD COLUMN person_id TEXT REFERENCES persons(id) ON DELETE CASCADE`,
	`CREATE TABLE securities (id TEXT PRIMARY KEY, display_name TEXT NOT NULL)`,
	`CREATE TABLE security_identifiers (
		security_id TEXT NOT NULL REFERENCES securities(id) ON DELETE CASCADE,
		kind TEXT NOT NULL,
		value TEXT NOT NULL,
		PRIMARY KEY (security_id, kind, value))`,
	`CREATE UNIQUE INDEX idx_security_identifiers_unique
		ON security_identifiers(kind, value) WHERE kind IN ('ISIN', 'WKN')`,
	`CREATE TABLE listings (
		id TEXT PRIMARY KEY,
		security_id TEXT NOT NULL REFERENCES securities(id) ON DELETE CASCADE,
		exchange TEXT, ticker TEXT NOT NULL, currency TEXT NOT NULL, quote_provider TEXT,
		UNIQUE (security_id, ticker))`,
	`CREATE TABLE quotes (
		listing_id TEXT NOT NULL REFERENCES listings(id) ON DELETE CASCADE,
		time TIMESTAMP NOT NULL,
		price INTEGER NOT NULL,
		PRIMARY KEY (listing_id, time))`,
	`CREATE TABLE transactions (
		id TEXT PRIMARY KEY,
		type TEXT NOT NULL CHECK (type IN ('BUY','SELL','DIVIDEND','DEPOSIT_CASH','WITHDRAW_CASH')),
		time TIMESTAMP NOT NULL,
		security_id TEXT REFERENCES securities(id) ON DELETE RESTRICT,
		cash_account_id TEXT REFERENCES cash_accounts(id) ON DELETE CASCADE,
		units REAL NOT NULL DEFAULT 0,
		price INTEGER,
		fees INTEGER NOT NULL DEFAULT 0,
		taxes INTEGER NOT NULL DEFAULT 0,
		cash_delta INTEGER NOT NULL DEFAULT 0,
		currency TEXT NOT NULL,
		source TEXT NOT NULL DEFAULT 'MANUAL',
		created_at TIMESTAMP NOT NULL DEFAULT now())`,
	`CREATE INDEX idx_transactions_cash_account_time ON transactions(cash_account_id, time)`,
}

// TestPortExecution runs money-gopher's real queries with real arguments and
// checks the answers.
//
// Preparing a statement only proves it binds. This exercises the rest: the
// parameters convert, the rows come back, and they scan into the Go types the
// generated code uses.
func TestPortExecution(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, portSchema...)

	// GrantPersonAccess: ON CONFLICT DO NOTHING, run twice.
	mustExecAll(t, db,
		`INSERT INTO users (id, issuer, subject, display_name) VALUES ('u1', 'iss', 'sub', 'Ada')`,
		`INSERT INTO persons (id, display_name) VALUES ('p1', 'Ada Lovelace')`,
	)
	for range 2 {
		if _, err := db.Exec(
			`INSERT INTO user_person_access (user_id, person_id) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			"u1", "p1"); err != nil {
			t.Fatalf("GrantPersonAccess: %v", err)
		}
	}
	if got := rowsOf(t, db, `SELECT count(*) FROM user_person_access`); !slices.Equal(got, []string{"1"}) {
		t.Errorf("the second grant should have been a no-op, got %v", got)
	}

	// ListPersonsForUser: a join, scanned into the generated struct's fields.
	t.Run("ListPersonsForUser", func(t *testing.T) {
		rows, err := db.Query(`SELECT p.* FROM persons p
			JOIN user_person_access upa ON upa.person_id = p.id
			WHERE upa.user_id = ? ORDER BY p.display_name`, "u1")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()

		var got []string
		for rows.Next() {
			var id, name string
			var created time.Time
			if err := rows.Scan(&id, &name, &created); err != nil {
				t.Fatal(err)
			}
			if created.IsZero() {
				t.Error("created_at defaulted to the zero time; now() did not apply")
			}
			got = append(got, id+"/"+name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if want := []string{"p1/Ada Lovelace"}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// CreateCashAccount: INSERT ... RETURNING *, with a nullable column.
	t.Run("CreateCashAccount", func(t *testing.T) {
		var id, name, currency string
		var iban sql.NullString
		var created time.Time
		var personID sql.NullString
		err := db.QueryRow(
			`INSERT INTO cash_accounts (id, person_id, display_name, currency, iban)
			 VALUES (?, ?, ?, ?, ?) RETURNING *`,
			"ca1", "p1", "Main", "EUR", "DE123").
			Scan(&id, &name, &currency, &iban, &created, &personID)
		if err != nil {
			t.Fatal(err)
		}
		if id != "ca1" || !iban.Valid || iban.String != "DE123" {
			t.Errorf("returned id=%q iban=%v", id, iban)
		}
		// person_id was added by ALTER, so it is the last column and must come
		// back with the value just written rather than as its missing value.
		if !personID.Valid || personID.String != "p1" {
			t.Errorf("person_id = %v, want p1", personID)
		}
	})

	// GetCashAccountBalance: CAST(COALESCE(SUM(...), 0) AS INTEGER).
	t.Run("GetCashAccountBalance", func(t *testing.T) {
		var balance int64
		q := `SELECT CAST(COALESCE(SUM(cash_delta), 0) AS INTEGER) AS balance
		      FROM transactions WHERE cash_account_id = ?`
		// No rows yet: COALESCE has to turn the NULL sum into 0.
		if err := db.QueryRow(q, "ca1").Scan(&balance); err != nil {
			t.Fatal(err)
		}
		if balance != 0 {
			t.Errorf("empty balance = %d, want 0", balance)
		}

		mustExecAll(t, db,
			`INSERT INTO transactions (id, type, time, cash_account_id, cash_delta, currency)
			 VALUES ('t1', 'DEPOSIT_CASH', '2024-01-01 10:00:00', 'ca1', 5000, 'EUR')`,
			`INSERT INTO transactions (id, type, time, cash_account_id, cash_delta, currency)
			 VALUES ('t2', 'WITHDRAW_CASH', '2024-02-01 10:00:00', 'ca1', -1500, 'EUR')`,
		)
		if err := db.QueryRow(q, "ca1").Scan(&balance); err != nil {
			t.Fatal(err)
		}
		if balance != 3500 {
			t.Errorf("balance = %d, want 3500", balance)
		}
	})

	// CreateQuote: the upsert, with a time.Time key.
	t.Run("CreateQuote", func(t *testing.T) {
		mustExecAll(t, db,
			`INSERT INTO securities VALUES ('s1', 'ACME')`,
			`INSERT INTO listings VALUES ('l1', 's1', 'XETRA', 'ACM', 'EUR', 'p')`,
		)
		at := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
		q := `INSERT INTO quotes (listing_id, time, price) VALUES (?, ?, ?)
		      ON CONFLICT (listing_id, time) DO UPDATE SET price = excluded.price`
		if _, err := db.Exec(q, "l1", at, 100); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(q, "l1", at, 250); err != nil {
			t.Fatal(err)
		}

		var price int64
		var got time.Time
		if err := db.QueryRow(
			`SELECT time, price FROM quotes WHERE listing_id = ? ORDER BY time DESC LIMIT 1`, "l1").
			Scan(&got, &price); err != nil {
			t.Fatal(err)
		}
		if price != 250 {
			t.Errorf("price = %d, want 250 (the upsert should have replaced it)", price)
		}
		if !got.Equal(at) {
			t.Errorf("time = %v, want %v", got, at)
		}
		if n := rowsOf(t, db, `SELECT count(*) FROM quotes`); !slices.Equal(n, []string{"1"}) {
			t.Errorf("the upsert inserted a second row: %v", n)
		}
	})

	// The partial unique index enforces the ISIN rule while leaving tickers free.
	t.Run("security identifiers", func(t *testing.T) {
		mustExecAll(t, db, `INSERT INTO security_identifiers VALUES ('s1', 'ISIN', 'DE0001')`)
		mustExecAll(t, db, `INSERT INTO securities VALUES ('s2', 'OTHER')`)
		if err := queryErr(db, `INSERT INTO security_identifiers VALUES ('s2', 'ISIN', 'DE0001')`); err == nil {
			t.Error("a duplicate ISIN across securities was accepted")
		}
		mustExecAll(t, db,
			`INSERT INTO security_identifiers VALUES ('s1', 'TICKER', 'ACM')`,
			`INSERT INTO security_identifiers VALUES ('s2', 'TICKER', 'ACM')`,
		)
	})

	// ON DELETE CASCADE through the column added by ALTER TABLE.
	t.Run("cascade through an added column", func(t *testing.T) {
		if _, err := db.Exec(`DELETE FROM persons WHERE id = 'p1'`); err != nil {
			t.Fatal(err)
		}
		if got := rowsOf(t, db, `SELECT count(*) FROM cash_accounts`); !slices.Equal(got, []string{"0"}) {
			t.Errorf("the cash account should have cascaded away, got %v", got)
		}
		// And its transactions with it, one level further down.
		if got := rowsOf(t, db, `SELECT count(*) FROM transactions`); !slices.Equal(got, []string{"0"}) {
			t.Errorf("transactions should have cascaded too, got %v", got)
		}
	})
}
