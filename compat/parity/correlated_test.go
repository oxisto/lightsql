//go:build parity

package parity

import "testing"

// TestCorrelatedSubqueries covers the shape an ORM emits constantly: a
// subquery that reads a column of the query containing it.
func TestCorrelatedSubqueries(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE authors (id INT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE books (id INT PRIMARY KEY, author_id INT, title TEXT, year INT)`,
		`INSERT INTO authors VALUES (1, 'Ada'), (2, 'Bob'), (3, 'Cy'), (4, NULL)`,
		`INSERT INTO books VALUES
			(10, 1, 'x', 2001), (11, 1, 'y', 2004), (12, 2, 'z', 1999), (13, NULL, 'orphan', 2000)`,
	)
	p.checkAll(t,
		// The canonical existence check.
		`SELECT name FROM authors a WHERE EXISTS (SELECT 1 FROM books b WHERE b.author_id = a.id) ORDER BY name`,
		`SELECT name FROM authors a WHERE NOT EXISTS (SELECT 1 FROM books b WHERE b.author_id = a.id) ORDER BY name`,
		// A correlated scalar subquery in the select list, which has a
		// different answer per row -- the thing an uncorrelated one cannot do.
		`SELECT a.id, (SELECT count(*) FROM books b WHERE b.author_id = a.id) FROM authors a ORDER BY a.id`,
		`SELECT a.id, (SELECT max(b.year) FROM books b WHERE b.author_id = a.id) FROM authors a ORDER BY a.id`,
		// No matching row is NULL, not zero and not an error.
		`SELECT a.id, (SELECT b.title FROM books b WHERE b.author_id = a.id AND b.year > 3000) FROM authors a ORDER BY a.id`,
		// Correlated IN.
		`SELECT name FROM authors a WHERE a.id IN (SELECT b.author_id FROM books b WHERE b.year > 2000) ORDER BY name`,
		`SELECT name FROM authors a WHERE a.id NOT IN (SELECT b.author_id FROM books b WHERE b.year > 2000) ORDER BY name`,
		// In a WHERE that also filters, and against a NULL outer value.
		`SELECT count(*) FROM authors a WHERE a.name IS NOT NULL AND EXISTS (SELECT 1 FROM books b WHERE b.author_id = a.id)`,
		`SELECT count(*) FROM authors a WHERE EXISTS (SELECT 1 FROM books b WHERE b.author_id = a.id AND a.name IS NULL)`,
		// Correlated on a comparison other than equality.
		`SELECT a.id FROM authors a WHERE EXISTS (SELECT 1 FROM books b WHERE b.author_id > a.id) ORDER BY a.id`,
	)
}

// TestCorrelationDepth covers a subquery reading two levels out, where the
// middle query has to carry the value down rather than resolving it itself.
func TestCorrelationDepth(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE a (id INT)`,
		`CREATE TABLE b (id INT, a_id INT)`,
		`CREATE TABLE c (id INT, b_id INT)`,
		`INSERT INTO a VALUES (1), (2)`,
		`INSERT INTO b VALUES (10, 1), (11, 2)`,
		`INSERT INTO c VALUES (100, 10)`,
	)
	p.checkAll(t,
		`SELECT a.id FROM a WHERE EXISTS (
			SELECT 1 FROM b WHERE b.a_id = a.id AND EXISTS (
				SELECT 1 FROM c WHERE c.b_id = b.id)) ORDER BY a.id`,
		// The innermost reaches past its parent to the outermost query.
		`SELECT a.id FROM a WHERE EXISTS (
			SELECT 1 FROM b WHERE EXISTS (
				SELECT 1 FROM c WHERE c.b_id = b.id AND b.a_id = a.id)) ORDER BY a.id`,
	)
}

// TestCorrelationShadowing pins that an inner name wins. Resolving outward only
// when the inner query does not have the name is what makes a subquery mean the
// same thing whether or not the query around it happens to share a column.
func TestCorrelationShadowing(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE outer_t (id INT, v INT)`,
		`CREATE TABLE inner_t (id INT, v INT)`,
		`INSERT INTO outer_t VALUES (1, 100), (2, 200)`,
		`INSERT INTO inner_t VALUES (1, 100), (2, 999)`,
	)
	p.checkAll(t,
		// v is defined in both, so the unqualified one is the inner.
		`SELECT o.id FROM outer_t o WHERE EXISTS (SELECT 1 FROM inner_t WHERE v = 999) ORDER BY o.id`,
		// Qualifying reaches the outer one explicitly.
		`SELECT o.id FROM outer_t o WHERE EXISTS (SELECT 1 FROM inner_t i WHERE i.v = o.v) ORDER BY o.id`,
	)
}
