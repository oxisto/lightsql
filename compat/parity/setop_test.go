//go:build parity

package parity

import "testing"

// TestSetOperations covers UNION, INTERSECT and EXCEPT, including the ALL forms
// where the answer depends on how many copies of a row each side had rather
// than merely whether it had one.
func TestSetOperations(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE a (v INT)`,
		`CREATE TABLE b (v INT)`,
		`INSERT INTO a VALUES (1), (1), (1), (2), (3), (NULL)`,
		`INSERT INTO b VALUES (1), (3), (4), (NULL)`,
	)
	p.checkAll(t,
		`SELECT v FROM a UNION SELECT v FROM b ORDER BY v`,
		`SELECT v FROM a UNION ALL SELECT v FROM b ORDER BY v`,
		`SELECT v FROM a INTERSECT SELECT v FROM b ORDER BY v`,
		`SELECT v FROM a INTERSECT ALL SELECT v FROM b ORDER BY v`,
		`SELECT v FROM a EXCEPT SELECT v FROM b ORDER BY v`,
		`SELECT v FROM a EXCEPT ALL SELECT v FROM b ORDER BY v`,
		// NULL is one value to a set operation, not the unknown it is to a
		// comparison: two NULLs are the same row.
		`SELECT count(*) FROM (SELECT v FROM a UNION SELECT v FROM b) u`,
		`SELECT count(*) FROM (SELECT v FROM a INTERSECT SELECT v FROM b) u`,
	)
}

// TestSetOperationPrecedence pins the associativity, which is not what reading
// the words left to right suggests: INTERSECT binds tighter than UNION and
// EXCEPT, so a UNION b INTERSECT c is a UNION (b INTERSECT c).
func TestSetOperationPrecedence(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE a (v INT)`, `CREATE TABLE b (v INT)`, `CREATE TABLE c (v INT)`,
		`INSERT INTO a VALUES (1), (2)`,
		`INSERT INTO b VALUES (2), (3)`,
		`INSERT INTO c VALUES (3), (4)`,
	)
	p.checkAll(t,
		`SELECT v FROM a UNION SELECT v FROM b INTERSECT SELECT v FROM c ORDER BY v`,
		`(SELECT v FROM a UNION SELECT v FROM b) INTERSECT SELECT v FROM c ORDER BY v`,
		// UNION and EXCEPT share a precedence and go left to right.
		`SELECT v FROM a UNION SELECT v FROM b EXCEPT SELECT v FROM c ORDER BY v`,
		`SELECT v FROM a EXCEPT SELECT v FROM b UNION SELECT v FROM c ORDER BY v`,
		`SELECT v FROM a INTERSECT SELECT v FROM b INTERSECT SELECT v FROM c ORDER BY v`,
	)
}

// TestSetOperationClauses covers where ORDER BY, LIMIT and OFFSET attach: to
// the whole operation, not to the arm they follow.
func TestSetOperationClauses(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE a (v INT, s TEXT)`,
		`CREATE TABLE b (v INT, s TEXT)`,
		`INSERT INTO a VALUES (3, 'c'), (1, 'a')`,
		`INSERT INTO b VALUES (2, 'b'), (4, 'd')`,
	)
	p.checkAll(t,
		`SELECT v FROM a UNION SELECT v FROM b ORDER BY v DESC`,
		`SELECT v FROM a UNION SELECT v FROM b ORDER BY 1 LIMIT 2`,
		`SELECT v FROM a UNION SELECT v FROM b ORDER BY v OFFSET 1`,
		`SELECT v FROM a UNION SELECT v FROM b ORDER BY v LIMIT 2 OFFSET 1`,
		// Ordering by the output column's name, which comes from the left arm.
		`SELECT v AS n FROM a UNION SELECT v FROM b ORDER BY n`,
		// A parenthesised arm may carry its own LIMIT.
		`(SELECT v FROM a ORDER BY v LIMIT 1) UNION (SELECT v FROM b ORDER BY v LIMIT 1) ORDER BY v`,
	)
}

// TestSetOperationTypes covers the arms having to agree, and what the result's
// type is when they differ but are compatible.
func TestSetOperationTypes(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE i (v INT)`,
		`CREATE TABLE n (v NUMERIC(10,2))`,
		`CREATE TABLE s (v TEXT)`,
		`INSERT INTO i VALUES (1), (2)`,
		`INSERT INTO n VALUES (1.50), (2.00)`,
		`INSERT INTO s VALUES ('x')`,
	)
	p.checkAll(t,
		// An integer and a numeric meet as numeric, exactly.
		`SELECT v FROM i UNION SELECT v FROM n ORDER BY v`,
		`SELECT v FROM i UNION ALL SELECT v FROM n ORDER BY v`,
		// A NULL takes the other side's type.
		`SELECT v FROM i UNION SELECT NULL ORDER BY v`,
		// Refused by both, with the same code: different column counts, and
		// types that cannot be matched.
		`SELECT v FROM i UNION SELECT v, v FROM i`,
		`SELECT v FROM i UNION SELECT v FROM s`,
		// The names come from the left arm.
		`SELECT v AS left_name FROM i UNION SELECT v FROM i ORDER BY left_name`,
	)
}

// TestSetOperationsCompose covers a set operation appearing where a SELECT can:
// in a derived table, in a subquery, and as the source of an INSERT.
func TestSetOperationsCompose(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE a (v INT)`,
		`CREATE TABLE b (v INT)`,
		`CREATE TABLE dest (v INT)`,
		`INSERT INTO a VALUES (1), (2)`,
		`INSERT INTO b VALUES (2), (3)`,
	)
	p.checkAll(t,
		`SELECT count(*) FROM (SELECT v FROM a UNION SELECT v FROM b) u`,
		`SELECT v FROM (SELECT v FROM a UNION SELECT v FROM b) u WHERE v > 1 ORDER BY v`,
		`SELECT v FROM a WHERE v IN (SELECT v FROM a INTERSECT SELECT v FROM b) ORDER BY v`,
		`SELECT count(*) FROM a WHERE EXISTS (SELECT v FROM a EXCEPT SELECT v FROM b)`,
		`INSERT INTO dest SELECT v FROM a UNION SELECT v FROM b RETURNING v`,
		`SELECT count(*) FROM dest`,
		// Grouping over the result of a set operation.
		`SELECT v, count(*) FROM (SELECT v FROM a UNION ALL SELECT v FROM b) u GROUP BY v ORDER BY v`,
	)
}
