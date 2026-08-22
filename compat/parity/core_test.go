//go:build parity

package parity

import "testing"

// TestThreeValuedLogic is the bug class the value model was built to prevent,
// so it is the one most worth checking against the real thing rather than
// against a truth table someone typed out.
func TestThreeValuedLogic(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, a INT, b BOOLEAN, s TEXT)`,
		`INSERT INTO t VALUES (1, 1, true, 'x'), (2, NULL, NULL, NULL), (3, 0, false, '')`,
	)
	p.checkAll(t,
		`SELECT id, a = 1, a <> 1, a > 0, a IS NULL, a IS NOT NULL FROM t ORDER BY id`,
		`SELECT id, b AND true, b OR true, b AND false, b OR false, NOT b FROM t ORDER BY id`,
		`SELECT id, a IS DISTINCT FROM 1, a IS NOT DISTINCT FROM NULL FROM t ORDER BY id`,
		`SELECT count(*) FROM t WHERE a = NULL`,
		`SELECT count(*) FROM t WHERE a IS NULL`,
		`SELECT count(*) FROM t WHERE NOT (a = 1)`,
		`SELECT count(*) FROM t WHERE a IN (1, 2)`,
		`SELECT count(*) FROM t WHERE a IN (1, NULL)`,
		`SELECT count(*) FROM t WHERE a NOT IN (1, NULL)`,
		`SELECT NULL = NULL, NULL <> NULL, NULL AND true, NULL OR true`,
	)
}

// TestOrdering covers where NULLs sort and how ties break, which is the sort of
// thing that is easy to get almost right.
func TestOrdering(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, a INT, s TEXT)`,
		`INSERT INTO t VALUES (1, 2, 'b'), (2, NULL, 'a'), (3, 1, NULL), (4, 2, 'a')`,
	)
	p.checkAll(t,
		`SELECT id FROM t ORDER BY a`,
		`SELECT id FROM t ORDER BY a DESC`,
		`SELECT id FROM t ORDER BY a NULLS FIRST`,
		`SELECT id FROM t ORDER BY a DESC NULLS LAST`,
		`SELECT id FROM t ORDER BY a, id`,
		`SELECT id FROM t ORDER BY s`,
		`SELECT id FROM t ORDER BY 2, 1`,
		`SELECT id FROM t ORDER BY a LIMIT 2`,
		`SELECT id FROM t ORDER BY a OFFSET 1`,
		`SELECT DISTINCT a FROM t ORDER BY a`,
	)
}

// TestAggregatesOverNothing pins the rules that catch people out: count is zero
// where the rest are NULL.
func TestAggregatesOverNothing(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, a INT, g TEXT)`,
		`INSERT INTO t VALUES (1, 1, 'x'), (2, NULL, 'x'), (3, 3, 'y')`,
	)
	p.checkAll(t,
		`SELECT count(*), count(a), sum(a), avg(a), min(a), max(a) FROM t WHERE id < 0`,
		`SELECT count(*), count(a), sum(a), min(a), max(a) FROM t`,
		`SELECT g, count(*), sum(a) FROM t GROUP BY g ORDER BY g`,
		`SELECT g, count(*) FROM t GROUP BY g HAVING count(*) > 1 ORDER BY g`,
		`SELECT count(DISTINCT g) FROM t`,
		`SELECT sum(a) + 1 FROM t`,
	)
}

// TestStringsAndPatterns covers the functions and the LIKE operator, where an
// earlier bug made every pattern answer the float 1.
func TestStringsAndPatterns(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, s TEXT)`,
		`INSERT INTO t VALUES (1, 'Hello'), (2, '  pad  '), (3, '100%'), (4, 'a_b'), (5, NULL)`,
	)
	p.checkAll(t,
		`SELECT id, lower(s), upper(s), length(s), trim(s) FROM t ORDER BY id`,
		`SELECT id, octet_length(s) FROM t ORDER BY id`,
		`SELECT id FROM t WHERE s LIKE 'H%' ORDER BY id`,
		`SELECT id FROM t WHERE s LIKE '%o' ORDER BY id`,
		`SELECT id FROM t WHERE s LIKE '100\%' ORDER BY id`,
		`SELECT id FROM t WHERE s LIKE 'a_b' ORDER BY id`,
		`SELECT id FROM t WHERE s LIKE 'a\_b' ORDER BY id`,
		`SELECT id FROM t WHERE s NOT LIKE 'H%' ORDER BY id`,
		`SELECT 'a' || 'b', 'a' || NULL`,
		`SELECT coalesce(s, 'none') FROM t ORDER BY id`,
		`SELECT nullif(s, 'Hello') FROM t ORDER BY id`,
		`SELECT s FROM t ORDER BY s`,
	)
}

// TestCaseAndCast covers the two constructs where a type has to be decided from
// several candidates.
func TestCaseAndCast(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, a INT)`,
		`INSERT INTO t VALUES (1, 1), (2, 2), (3, NULL)`,
	)
	p.checkAll(t,
		`SELECT id, CASE WHEN a = 1 THEN 'one' WHEN a = 2 THEN 'two' ELSE 'other' END FROM t ORDER BY id`,
		`SELECT id, CASE a WHEN 1 THEN 'one' ELSE 'other' END FROM t ORDER BY id`,
		`SELECT id, CASE WHEN a = 1 THEN 'one' END FROM t ORDER BY id`,
		`SELECT CAST('42' AS INTEGER), CAST(42 AS TEXT), CAST('1.5' AS NUMERIC)`,
		`SELECT CAST(1.7 AS INTEGER), CAST(-1.7 AS INTEGER), CAST(2.5 AS INTEGER)`,
		`SELECT CAST('t' AS BOOLEAN), CAST(1 AS BOOLEAN)`,
		`SELECT CAST('nope' AS INTEGER)`,
		`SELECT 1::text, '2'::int`,
		`SELECT coalesce(a, 0) FROM t ORDER BY id`,
	)
}

// TestConstraintErrors compares failures, since matching only on success would
// make an engine that never refuses anything look perfect.
func TestConstraintErrors(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE parent (id INT PRIMARY KEY)`,
		`CREATE TABLE child (
			id INT PRIMARY KEY,
			p INT REFERENCES parent(id),
			n INT NOT NULL,
			u INT UNIQUE,
			c INT CHECK (c > 0)
		)`,
		`INSERT INTO parent VALUES (1)`,
		`INSERT INTO child VALUES (1, 1, 1, 1, 1)`,
	)
	p.checkAll(t,
		`INSERT INTO child VALUES (1, 1, 1, 2, 1) RETURNING id`,
		`INSERT INTO child VALUES (2, 99, 1, 2, 1) RETURNING id`,
		`INSERT INTO child (id, p, u, c) VALUES (3, 1, 3, 1) RETURNING id`,
		`INSERT INTO child VALUES (4, 1, 1, 1, 1) RETURNING id`,
		`INSERT INTO child VALUES (5, 1, 1, 5, 0) RETURNING id`,
		`SELECT * FROM nosuchtable`,
		`SELECT nosuchcolumn FROM child`,
		`SELECT nosuchfunction(1)`,
	)
}

// TestJoins covers every join type against the same fixture, including the
// unmatched sides an outer join has to pad.
func TestJoins(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE l (id INT PRIMARY KEY, v TEXT)`,
		`CREATE TABLE r (id INT PRIMARY KEY, lid INT, v TEXT)`,
		`INSERT INTO l VALUES (1, 'a'), (2, 'b'), (3, 'c')`,
		`INSERT INTO r VALUES (10, 1, 'x'), (11, 1, 'y'), (12, 99, 'z')`,
	)
	p.checkAll(t,
		`SELECT l.id, r.id FROM l JOIN r ON r.lid = l.id ORDER BY l.id, r.id`,
		`SELECT l.id, r.id FROM l LEFT JOIN r ON r.lid = l.id ORDER BY l.id, r.id`,
		`SELECT l.id, r.id FROM l RIGHT JOIN r ON r.lid = l.id ORDER BY l.id, r.id`,
		`SELECT l.id, r.id FROM l FULL JOIN r ON r.lid = l.id ORDER BY l.id, r.id`,
		`SELECT count(*) FROM l CROSS JOIN r`,
		`SELECT count(*) FROM l, r`,
		`SELECT l.v, count(r.id) FROM l LEFT JOIN r ON r.lid = l.id GROUP BY l.v ORDER BY l.v`,
	)
}

// TestSubqueries covers the three forms, including what a scalar subquery does
// when it matches nothing.
func TestSubqueries(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, a INT)`,
		`INSERT INTO t VALUES (1, 10), (2, 20), (3, NULL)`,
	)
	p.checkAll(t,
		`SELECT (SELECT max(a) FROM t)`,
		`SELECT (SELECT a FROM t WHERE id = 99)`,
		`SELECT id FROM t WHERE a IN (SELECT a FROM t WHERE a > 15) ORDER BY id`,
		`SELECT id FROM t WHERE EXISTS (SELECT 1 FROM t WHERE a > 15) ORDER BY id`,
		`SELECT id FROM t WHERE NOT EXISTS (SELECT 1 FROM t WHERE a > 99) ORDER BY id`,
		`SELECT x.id FROM (SELECT id, a FROM t WHERE a IS NOT NULL) x ORDER BY x.id`,
		`SELECT count(*) FROM t WHERE a > (SELECT avg(a) FROM t)`,
	)
}
