package driver_test

import (
	"database/sql"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// TestCascadeRevalidatesTheChild is the regression test for referential actions
// that wrote to a child table without anything re-checking the child's own
// constraints.
//
// SET DEFAULT was the worst case: it could leave a row that violated a CHECK
// and pointed at a parent that did not exist, so the foreign-key machinery
// broke the very constraint it exists to maintain.
func TestCascadeRevalidatesTheChild(t *testing.T) {
	t.Run("SET DEFAULT that breaks a CHECK", func(t *testing.T) {
		db := open(t)
		mustExecAll(t, db,
			`CREATE TABLE p (id INT PRIMARY KEY)`,
			`CREATE TABLE c (id INT PRIMARY KEY,
				pid INT DEFAULT 0 REFERENCES p (id) ON DELETE SET DEFAULT,
				CHECK (pid > 0))`,
			`INSERT INTO p VALUES (1)`,
			`INSERT INTO c VALUES (10, 1)`,
		)

		if _, err := db.Exec(`DELETE FROM p WHERE id = 1`); err == nil {
			var pid int
			db.QueryRow(`SELECT pid FROM c WHERE id = 10`).Scan(&pid)
			t.Fatalf("delete succeeded, leaving c.pid = %d against CHECK (pid > 0)", pid)
		}

		// The failed statement must leave both tables as they were.
		var pid int
		if err := db.QueryRow(`SELECT pid FROM c WHERE id = 10`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if pid != 1 {
			t.Errorf("c.pid = %d after a rejected delete, want 1", pid)
		}
		var id int
		if err := db.QueryRow(`SELECT id FROM p WHERE id = 1`).Scan(&id); err != nil {
			t.Errorf("parent row was removed by a rejected delete: %v", err)
		}
	})

	t.Run("SET DEFAULT that points at no parent", func(t *testing.T) {
		db := open(t)
		mustExecAll(t, db,
			`CREATE TABLE p (id INT PRIMARY KEY)`,
			`CREATE TABLE c (id INT PRIMARY KEY,
				pid INT DEFAULT 99 REFERENCES p (id) ON DELETE SET DEFAULT)`,
			`INSERT INTO p VALUES (1)`,
			`INSERT INTO c VALUES (10, 1)`,
		)
		// There is no parent with id 99, so the default is not a valid
		// reference and the delete has to fail.
		if _, err := db.Exec(`DELETE FROM p WHERE id = 1`); err == nil {
			t.Fatal("delete succeeded, leaving c.pid dangling at 99")
		} else if got := sqlstate(err); got != "23503" {
			t.Errorf("SQLSTATE = %q, want 23503 (foreign_key_violation)", got)
		}
	})

	t.Run("SET NULL against NOT NULL", func(t *testing.T) {
		db := open(t)
		mustExecAll(t, db,
			`CREATE TABLE p (id INT PRIMARY KEY)`,
			`CREATE TABLE c (id INT PRIMARY KEY,
				pid INT NOT NULL REFERENCES p (id) ON DELETE SET NULL)`,
			`INSERT INTO p VALUES (1)`,
			`INSERT INTO c VALUES (10, 1)`,
		)
		if _, err := db.Exec(`DELETE FROM p WHERE id = 1`); err == nil {
			t.Fatal("delete succeeded, setting a NOT NULL column to NULL")
		}
	})

	// The check must not reject a cascade that is actually fine, or every
	// referential action would become unusable.
	t.Run("a valid cascade still succeeds", func(t *testing.T) {
		db := open(t)
		mustExecAll(t, db,
			`CREATE TABLE p (id INT PRIMARY KEY)`,
			`CREATE TABLE c (id INT PRIMARY KEY,
				pid INT REFERENCES p (id) ON DELETE SET NULL)`,
			`INSERT INTO p VALUES (1)`,
			`INSERT INTO c VALUES (10, 1)`,
		)
		if _, err := db.Exec(`DELETE FROM p WHERE id = 1`); err != nil {
			t.Fatalf("a legal SET NULL was rejected: %v", err)
		}
		var pid any
		if err := db.QueryRow(`SELECT pid FROM c WHERE id = 10`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if pid != nil {
			t.Errorf("c.pid = %v, want NULL", pid)
		}
	})
}

// TestConcurrentCreateAndDeleteOnAParent is the regression test for the data
// race on a table's incoming references.
//
// The binder appended to the slice while binding a child's CREATE TABLE, and
// the executor read it while applying referential actions. A shared engine
// behind database/sql's connection pool puts those on different goroutines, so
// this failed under -race. It is the reason the slice is now unexported and
// reached only under its own lock.
func TestConcurrentCreateAndDeleteOnAParent(t *testing.T) {
	db := open(t)
	db.SetMaxOpenConns(8)

	mustExecAll(t, db, `CREATE TABLE p (id INT PRIMARY KEY)`)
	for i := range 400 {
		if _, err := db.Exec(`INSERT INTO p VALUES ($1)`, i); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 60 {
			db.Exec(fmt.Sprintf(`CREATE TABLE c%d (id INT, pid INT REFERENCES p (id))`, i))
		}
	}()
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				db.Exec(`DELETE FROM p WHERE id = $1`, g*100+i)
			}
		}()
	}
	wg.Wait()
}

// mustExecAll runs setup statements, failing the test on the first error.
func mustExecAll(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// TestCascadeRecurses covers a cascade reaching past the immediate children.
//
// Deleting a row used to remove its children and stop there, leaving the
// grandchildren pointing at rows that no longer existed -- referential
// integrity broken by the machinery meant to enforce it, and silently, since
// nothing revisited them afterwards.
func TestCascadeRecurses(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE a (id INT PRIMARY KEY)`,
		`CREATE TABLE b (id INT PRIMARY KEY, a_id INT REFERENCES a(id) ON DELETE CASCADE)`,
		`CREATE TABLE c (id INT PRIMARY KEY, b_id INT REFERENCES b(id) ON DELETE CASCADE)`,
		`CREATE TABLE d (id INT PRIMARY KEY, c_id INT REFERENCES c(id) ON DELETE CASCADE)`,
		`INSERT INTO a VALUES (1), (2)`,
		`INSERT INTO b VALUES (1, 1), (2, 2)`,
		`INSERT INTO c VALUES (1, 1), (2, 2)`,
		`INSERT INTO d VALUES (1, 1), (2, 2)`,
	)

	if _, err := db.Exec(`DELETE FROM a WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	// The whole chain below the deleted row goes, and the untouched one stays.
	for _, q := range []string{
		`SELECT id FROM b ORDER BY id`,
		`SELECT id FROM c ORDER BY id`,
		`SELECT id FROM d ORDER BY id`,
	} {
		if got := rowsOf(t, db, q); !slices.Equal(got, []string{"2"}) {
			t.Errorf("%s: got %v, want only the untouched row [2]", q, got)
		}
	}
}

// TestCascadeTerminatesOnACycle pins the guard rather than a behaviour. A table
// referencing itself makes the cascade graph cyclic, and recursion without a
// record of what it has already acted on would not come back.
func TestCascadeTerminatesOnACycle(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE n (id INT PRIMARY KEY, parent INT REFERENCES n(id) ON DELETE CASCADE)`,
		`INSERT INTO n VALUES (1, NULL)`,
		`INSERT INTO n VALUES (2, 1)`,
		`INSERT INTO n VALUES (3, 2)`,
		`INSERT INTO n VALUES (4, NULL)`,
	)

	if _, err := db.Exec(`DELETE FROM n WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	// The chain rooted at 1 goes; the unrelated row stays.
	if got := rowsOf(t, db, `SELECT id FROM n ORDER BY id`); !slices.Equal(got, []string{"4"}) {
		t.Errorf("got %v, want [4]", got)
	}
}
