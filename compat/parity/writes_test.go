//go:build parity

package parity

import "testing"

// TestWritesAndReturning covers the statements that change rows, whose row
// counts and RETURNING shapes are as much a part of the dialect as a SELECT.
func TestWritesAndReturning(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, a INT, s TEXT DEFAULT 'd')`,
		`INSERT INTO t VALUES (1, 10, 'x'), (2, 20, 'y'), (3, NULL, NULL)`,
	)
	p.checkAll(t,
		`INSERT INTO t (id, a) VALUES (4, 40) RETURNING id, a, s`,
		`INSERT INTO t (id, a) VALUES (5, 50), (6, 60) RETURNING id`,
		`UPDATE t SET a = a + 1 WHERE id = 1 RETURNING id, a`,
		`UPDATE t SET a = a + 1 WHERE id = 99 RETURNING id`,
		`UPDATE t SET s = DEFAULT WHERE id = 2 RETURNING id, s`,
		`DELETE FROM t WHERE id = 6 RETURNING id`,
		`DELETE FROM t WHERE id = 99 RETURNING id`,
		`SELECT id, a, s FROM t ORDER BY id`,
		// An insert that reads from a select.
		`INSERT INTO t (id, a) SELECT id + 100, a FROM t WHERE a IS NOT NULL RETURNING id`,
		`SELECT count(*) FROM t`,
	)
}

// TestOnConflict covers the upsert, including which row RETURNING reports when
// nothing was inserted.
func TestOnConflict(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, a INT, s TEXT)`,
		`INSERT INTO t VALUES (1, 10, 'x')`,
	)
	p.checkAll(t,
		`INSERT INTO t VALUES (1, 99, 'z') ON CONFLICT (id) DO NOTHING RETURNING id, a`,
		`INSERT INTO t VALUES (2, 20, 'y') ON CONFLICT (id) DO NOTHING RETURNING id, a`,
		`INSERT INTO t VALUES (1, 99, 'z') ON CONFLICT (id) DO UPDATE SET a = 99 RETURNING id, a`,
		`INSERT INTO t VALUES (1, 5, 'w') ON CONFLICT (id) DO UPDATE SET a = EXCLUDED.a RETURNING id, a`,
		`SELECT id, a, s FROM t ORDER BY id`,
	)
}

// TestReferentialActions covers what happens to a child row when its parent
// goes, which is the part of a foreign key most often got subtly wrong.
func TestReferentialActions(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE parent (id INT PRIMARY KEY)`,
		`CREATE TABLE casc (id INT PRIMARY KEY, p INT REFERENCES parent(id) ON DELETE CASCADE)`,
		`CREATE TABLE setnull (id INT PRIMARY KEY, p INT REFERENCES parent(id) ON DELETE SET NULL)`,
		`CREATE TABLE restrict_ (id INT PRIMARY KEY, p INT REFERENCES parent(id) ON DELETE RESTRICT)`,
		`INSERT INTO parent VALUES (1), (2), (3)`,
		`INSERT INTO casc VALUES (10, 1)`,
		`INSERT INTO setnull VALUES (20, 2)`,
		`INSERT INTO restrict_ VALUES (30, 3)`,
	)
	p.checkAll(t,
		`DELETE FROM parent WHERE id = 1 RETURNING id`,
		`SELECT count(*) FROM casc`,
		`DELETE FROM parent WHERE id = 2 RETURNING id`,
		`SELECT id, p FROM setnull ORDER BY id`,
		// Refused by both, with the same code.
		`DELETE FROM parent WHERE id = 3 RETURNING id`,
		`SELECT count(*) FROM restrict_`,
	)
}

// TestTemporal covers the date and time types, where a payload read as the
// wrong unit produces a plausible answer rather than an error.
func TestTemporal(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, d DATE, ts TIMESTAMP, tz TIMESTAMPTZ)`,
		`INSERT INTO t VALUES
			(1, '2024-01-02', '2024-01-02 12:30:00', '2024-01-02 12:30:00+02:00'),
			(2, '1969-03-04', '1969-03-04 05:06:07', '1969-03-04 05:06:07Z'),
			(3, NULL, NULL, NULL)`,
	)
	p.checkAll(t,
		`SELECT id, d, ts FROM t ORDER BY id`,
		`SELECT id FROM t WHERE d = '2024-01-02'`,
		`SELECT id FROM t WHERE ts > '2000-01-01' ORDER BY id`,
		`SELECT id FROM t WHERE d < '1970-01-01' ORDER BY id`,
		`SELECT id FROM t ORDER BY d`,
		`SELECT min(d), max(d) FROM t`,
		`SELECT CAST('2024-01-02' AS DATE)`,
		`SELECT CAST('2024-01-02 12:30:00' AS TIMESTAMP)`,
		`SELECT CAST('not a date' AS DATE)`,
	)
}

// TestIntegerEdges covers division, modulo and the boundaries, which is where
// an engine that reaches for float64 quietly gives a different answer.
func TestIntegerEdges(t *testing.T) {
	p := open(t)
	p.checkAll(t,
		`SELECT 7 / 2, -7 / 2, 7 % 2, -7 % 2, 7 % -2`,
		`SELECT 9223372036854775807, -9223372036854775808`,
		`SELECT 2 ^ 10`,
		`SELECT abs(-5), abs(5), abs(-5.5)`,
		`SELECT round(2.5), round(-2.5), round(2.4)`,
		`SELECT 1 + NULL, NULL * 2`,
		`SELECT 5 > 3, 5 < 3, 5 >= 5, 'a' < 'b'`,
	)

	// PostgreSQL raises a numeric to a numeric power exactly, to sixteen
	// significant digits. lightsql computes it in float64, so the last digit
	// can differ. Doing better needs a logarithm and an exponential at
	// arbitrary precision, which is a great deal of machinery for an operator
	// that hardly appears in the queries this engine is for.
	p.checkKnown(t, "numeric exponentiation is computed in float64", `SELECT 2 ^ 0.5`)
}
