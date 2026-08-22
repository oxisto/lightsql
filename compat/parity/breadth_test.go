//go:build parity

package parity

import "testing"

// TestJSON covers the operators and the two document types, where lightsql
// keeps json as written and canonicalises jsonb -- a distinction it is easy to
// get subtly wrong in either direction.
func TestJSON(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, doc JSONB, raw JSON)`,
		`INSERT INTO t VALUES
			(1, '{"a": 1, "b": {"c": "x"}, "d": [10, 20]}', '{"a": 1, "b": {"c": "x"}}'),
			(2, '{"a": null}', '{"a": null}'),
			(3, NULL, NULL)`,
	)
	p.checkAll(t,
		`SELECT id, doc -> 'a', doc ->> 'a' FROM t ORDER BY id`,
		`SELECT id, doc -> 'b' -> 'c', doc -> 'b' ->> 'c' FROM t ORDER BY id`,
		`SELECT id, doc -> 'd' -> 0, doc -> 'd' ->> 1 FROM t ORDER BY id`,
		`SELECT id, doc -> 'missing', doc ->> 'missing' FROM t ORDER BY id`,
		`SELECT id FROM t WHERE doc ->> 'a' = '1' ORDER BY id`,
		`SELECT id FROM t WHERE doc @> '{"a": 1}' ORDER BY id`,
		`SELECT id, doc FROM t ORDER BY id`,
		`SELECT id, raw FROM t ORDER BY id`,
	)
}

// TestDistinctOn covers PostgreSQL's own extension, which picks the first row
// of each group and therefore depends entirely on the ordering.
func TestDistinctOn(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, g TEXT, v INT)`,
		`INSERT INTO t VALUES (1, 'a', 10), (2, 'a', 20), (3, 'b', 5), (4, 'b', NULL)`,
	)
	p.checkAll(t,
		`SELECT DISTINCT ON (g) g, v FROM t ORDER BY g, v`,
		`SELECT DISTINCT ON (g) g, v FROM t ORDER BY g, v DESC`,
		`SELECT DISTINCT g FROM t ORDER BY g`,
		`SELECT DISTINCT g, v FROM t ORDER BY g, v`,
		`SELECT count(DISTINCT g), count(DISTINCT v) FROM t`,
		`SELECT sum(DISTINCT v), avg(DISTINCT v) FROM t`,
	)
}

// TestGroupingEdges covers the parts of GROUP BY that are easy to get almost
// right: grouping on an expression, NULLs forming one group, and HAVING seeing
// an aggregate the select list does not.
func TestGroupingEdges(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, g TEXT, v INT)`,
		`INSERT INTO t VALUES (1, 'a', 1), (2, 'a', 2), (3, NULL, 3), (4, NULL, 4), (5, 'b', NULL)`,
	)
	p.checkAll(t,
		`SELECT g, count(*), sum(v) FROM t GROUP BY g ORDER BY g`,
		`SELECT g IS NULL, count(*) FROM t GROUP BY g IS NULL ORDER BY 1`,
		`SELECT v % 2, count(*) FROM t GROUP BY v % 2 ORDER BY 1`,
		`SELECT g FROM t GROUP BY g HAVING sum(v) > 2 ORDER BY g`,
		`SELECT g FROM t GROUP BY g HAVING count(*) = 1 ORDER BY g`,
		`SELECT count(*) FROM t GROUP BY g HAVING false`,
		`SELECT g, count(*) FROM t WHERE v IS NOT NULL GROUP BY g ORDER BY g`,
	)
}

// TestLimitOffset covers the boundaries, where an off-by-one is invisible until
// someone paginates.
func TestLimitOffset(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY)`,
		`INSERT INTO t VALUES (1), (2), (3), (4), (5)`,
	)
	p.checkAll(t,
		`SELECT id FROM t ORDER BY id LIMIT 0`,
		`SELECT id FROM t ORDER BY id LIMIT 2 OFFSET 0`,
		`SELECT id FROM t ORDER BY id LIMIT 2 OFFSET 3`,
		`SELECT id FROM t ORDER BY id LIMIT 2 OFFSET 99`,
		`SELECT id FROM t ORDER BY id OFFSET 4`,
		`SELECT id FROM t ORDER BY id LIMIT 99`,
	)
}

// TestSelfJoinAndAliases covers a table joined to itself, where every column
// needs qualifying and the scope has two of everything.
func TestSelfJoinAndAliases(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, parent INT, v TEXT)`,
		`INSERT INTO t VALUES (1, NULL, 'root'), (2, 1, 'child'), (3, 1, 'other'), (4, 99, 'orphan')`,
	)
	p.checkAll(t,
		`SELECT c.v, p.v FROM t c JOIN t p ON c.parent = p.id ORDER BY c.id`,
		`SELECT c.v, p.v FROM t c LEFT JOIN t p ON c.parent = p.id ORDER BY c.id`,
		`SELECT a.id, b.id FROM t a JOIN t b ON a.id < b.id ORDER BY a.id, b.id`,
		`SELECT t.id FROM t WHERE t.parent IS NULL ORDER BY t.id`,
		`SELECT x.v AS name FROM t AS x ORDER BY name`,
	)
}

// TestUsingAndNaturalShape covers JOIN ... USING, where the pair is merged into
// one column and SELECT * must show it once.
func TestUsingAndNaturalShape(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE l (id INT PRIMARY KEY, lv TEXT)`,
		`CREATE TABLE r (id INT PRIMARY KEY, rv TEXT)`,
		`INSERT INTO l VALUES (1, 'a'), (2, 'b')`,
		`INSERT INTO r VALUES (1, 'x'), (3, 'z')`,
	)
	p.checkAll(t,
		`SELECT * FROM l JOIN r USING (id) ORDER BY id`,
		`SELECT id, lv, rv FROM l JOIN r USING (id) ORDER BY id`,
		`SELECT id FROM l LEFT JOIN r USING (id) ORDER BY id`,
		`SELECT count(*) FROM l JOIN r USING (id)`,
	)
}

// TestTextEdges covers comparison and ordering of text, where a locale or a
// byte-versus-rune decision shows up as a different sort.
func TestTextEdges(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, s TEXT)`,
		`INSERT INTO t VALUES (1, 'a'), (2, 'A'), (3, ''), (4, '1'), (5, 'ä'), (6, 'z')`,
	)
	p.checkAll(t,
		`SELECT id, s FROM t ORDER BY s, id`,
		`SELECT id FROM t WHERE s > 'a' ORDER BY id`,
		`SELECT id FROM t WHERE s = '' ORDER BY id`,
		`SELECT length(s), octet_length(s) FROM t ORDER BY id`,
		`SELECT upper(s), lower(s) FROM t ORDER BY id`,
		`SELECT s || '!' FROM t ORDER BY id`,
	)
}

// TestNullPropagation covers where a NULL stops and where it spreads, which is
// the difference between a filter that matches nothing and one that errors.
func TestNullPropagation(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE t (id INT PRIMARY KEY, a INT, s TEXT)`,
		`INSERT INTO t VALUES (1, 1, 'x'), (2, NULL, NULL)`,
	)
	p.checkAll(t,
		`SELECT id, a + 1, a * 0, a / 1 FROM t ORDER BY id`,
		`SELECT id, upper(s), length(s), trim(s) FROM t ORDER BY id`,
		`SELECT id, coalesce(a, -1), coalesce(s, 'none') FROM t ORDER BY id`,
		`SELECT id, nullif(a, 1), nullif(s, 'x') FROM t ORDER BY id`,
		`SELECT id, s || 'y' FROM t ORDER BY id`,
		`SELECT id, CASE WHEN a IS NULL THEN 'null' ELSE 'set' END FROM t ORDER BY id`,
		`SELECT id, abs(a), round(a) FROM t ORDER BY id`,
	)
}
