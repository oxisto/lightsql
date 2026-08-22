package driver_test

import (
	"database/sql"
	"encoding/json"
	"testing"
)

// TestJSONBRoundTrip is the acceptance test for JSONB: a caller stores a
// document, gets it back, and unmarshals it — all through database/sql, which
// is the only way an application ever touches it.
func TestJSONBRoundTrip(t *testing.T) {
	db := open(t)

	if _, err := db.Exec(`CREATE TABLE docs (id INT PRIMARY KEY, doc JSONB)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	type payload struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	want := payload{Name: "widget", Tags: []string{"a", "b"}}

	// Marshalling to []byte and passing it as an argument is how an application
	// actually stores a document, so it has to work without a manual cast.
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO docs VALUES ($1, $2)`, 1, raw); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Scanning into []byte is what lets json.Unmarshal consume the result
	// directly, matching lib/pq and pgx.
	var out []byte
	if err := db.QueryRow(`SELECT doc FROM docs WHERE id = 1`).Scan(&out); err != nil {
		t.Fatalf("scan: %v", err)
	}
	var got payload
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if got.Name != want.Name || len(got.Tags) != len(want.Tags) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}

	// Scanning the same column into a string must also work, since database/sql
	// converts bytes to a string for free and callers rely on it.
	var s string
	if err := db.QueryRow(`SELECT doc FROM docs WHERE id = 1`).Scan(&s); err != nil {
		t.Fatalf("scan into string: %v", err)
	}
	if s != `{"name": "widget", "tags": ["a", "b"]}` {
		t.Errorf("stored form = %s, want canonical form", s)
	}
}

// TestJSONBIsCanonicalisedAndJSONIsNot is the observable difference between the
// two types, and the reason lightsql has both.
func TestJSONBIsCanonicalisedAndJSONIsNot(t *testing.T) {
	db := open(t)

	if _, err := db.Exec(`CREATE TABLE docs (b JSONB, j JSON)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	const written = `{ "b":1,  "a":2 }`
	if _, err := db.Exec(`INSERT INTO docs VALUES ($1, $1)`, written); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var gotB, gotJ string
	if err := db.QueryRow(`SELECT b, j FROM docs`).Scan(&gotB, &gotJ); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if gotB != `{"a": 2, "b": 1}` {
		t.Errorf("jsonb = %s, want keys sorted and whitespace dropped", gotB)
	}
	if gotJ != written {
		t.Errorf("json = %s, want the text exactly as written", gotJ)
	}
}

func TestJSONOperators(t *testing.T) {
	db := open(t)

	for _, stmt := range []string{
		`CREATE TABLE docs (id INT PRIMARY KEY, doc JSONB)`,
		`INSERT INTO docs VALUES (1, '{"user":{"name":"ada","age":36},"tags":["x","y"]}')`,
		`INSERT INTO docs VALUES (2, '{"user":{"name":"bob","age":41},"tags":["y"]}')`,
		`INSERT INTO docs VALUES (3, '{"user":{"name":"cy"}}')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	t.Run("chained lookup", func(t *testing.T) {
		var name string
		if err := db.QueryRow(`SELECT doc -> 'user' ->> 'name' FROM docs WHERE id = 1`).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != "ada" {
			t.Errorf("name = %q, want ada", name)
		}
	})

	t.Run("array element by index", func(t *testing.T) {
		var tag string
		if err := db.QueryRow(`SELECT doc -> 'tags' ->> 0 FROM docs WHERE id = 1`).Scan(&tag); err != nil {
			t.Fatal(err)
		}
		if tag != "x" {
			t.Errorf("tag = %q, want x", tag)
		}
	})

	t.Run("a missing key is NULL, not an error", func(t *testing.T) {
		var got sql.NullString
		if err := db.QueryRow(`SELECT doc -> 'user' ->> 'age' FROM docs WHERE id = 3`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got.Valid {
			t.Errorf("missing key = %q, want NULL", got.String)
		}
	})

	t.Run("used as a predicate", func(t *testing.T) {
		// The precedence case that matters in practice: this must group as
		// (doc -> 'user' ->> 'name') = 'bob'.
		var id int
		if err := db.QueryRow(`SELECT id FROM docs WHERE doc -> 'user' ->> 'name' = 'bob'`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id != 2 {
			t.Errorf("id = %d, want 2", id)
		}
	})

	t.Run("containment", func(t *testing.T) {
		rows, err := db.Query(`SELECT id FROM docs WHERE doc @> '{"tags":["y"]}' ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()

		var ids []int
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
			t.Errorf("ids = %v, want [1 2]", ids)
		}
	})

	t.Run("cast from a text literal", func(t *testing.T) {
		var got string
		if err := db.QueryRow(`SELECT '{"z":1,"a":2}'::jsonb ->> 'a'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != "2" {
			t.Errorf("got %q, want 2", got)
		}
	})
}

// TestMalformedJSONIsRejected checks that a document which cannot be read back
// never reaches storage, and that the error carries the SQLSTATE an application
// would match on.
func TestMalformedJSONIsRejected(t *testing.T) {
	db := open(t)

	if _, err := db.Exec(`CREATE TABLE docs (doc JSONB)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err := db.Exec(`INSERT INTO docs VALUES ('{not json')`)
	if err == nil {
		t.Fatal("malformed jsonb was accepted")
	}
	if got := sqlstate(err); got != "22P02" {
		t.Errorf("SQLSTATE = %q, want 22P02 (invalid_text_representation)", got)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM docs`).Scan(&n); err == nil && n != 0 {
		t.Errorf("table has %d rows, want none", n)
	}
}

// TestJSONBParameterIsCanonicalised checks that a document arriving as a
// parameter is canonicalised exactly as a literal is. Otherwise two equal
// documents would compare unequal depending only on how they reached the
// engine, which is the kind of difference a test suite would never think to
// look for.
func TestJSONBParameterIsCanonicalised(t *testing.T) {
	db := open(t)
	if _, err := db.Exec(`CREATE TABLE t (doc JSONB)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ($1)`, `{ "b":1, "a":2 }`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ('{"a":2,"b":1}')`); err != nil {
		t.Fatalf("insert literal: %v", err)
	}

	// Both rows must have become the same value, so the predicate matches
	// each of them.
	rows, err := db.Query(`SELECT doc FROM t WHERE doc = '{"a":2,"b":1}'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d of 2 rows matched; a parameter and a literal disagree", n)
	}
}
