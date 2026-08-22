//go:build parity

package parity

import "testing"

// TestNumericArithmetic is the case this suite was built for. Exact decimal
// arithmetic was implemented from a reading of PostgreSQL's source, and the
// scale its division picks is not something anyone should be confident about
// from reasoning alone -- an earlier attempt to predict 100/3 by hand was wrong
// by a decimal place.
func TestNumericArithmetic(t *testing.T) {
	p := open(t)
	p.checkAll(t,
		// The addition a float cannot do.
		`SELECT 0.1 + 0.2`,
		`SELECT 0.1 + 0.1 + 0.1 + 0.1 + 0.1 + 0.1 + 0.1 + 0.1 + 0.1 + 0.1`,
		// Scales add on multiplication and widen on addition.
		`SELECT 1.50 * 1.50`,
		`SELECT 1.50 + 1`,
		`SELECT 19.99 * 3`,
		`SELECT 100.00 - 99.99`,
		`SELECT -1.25`,
		// Division picks a scale, and which one is the whole question.
		`SELECT 1 / 3.0`,
		`SELECT 10 / 3.0`,
		`SELECT 100 / 3.0`,
		`SELECT 1 / 7.0`,
		`SELECT 7.5 / 2.5`,
		`SELECT 2 / 3.0`,
		`SELECT -2 / 3.0`,
		`SELECT 0.001 / 7`,
		`SELECT 1000000 / 7.0`,
		`SELECT 1.000000000000000000000 / 1`,
		// Rounding at the last place, which is half away from zero.
		`SELECT 2 / 3.0 = 0.66666666666666666667`,
		// Past what a float64 can represent at all.
		`SELECT 9007199254740993 + 0.000000000000001`,
		`SELECT 123456789012345678901234567890 * 2`,
		// Modulo, whose sign follows the dividend.
		`SELECT 7.5 % 2`,
		`SELECT -7.5 % 2`,
		// The errors have to match too.
		`SELECT 1 / 0`,
		`SELECT 1.0 / 0.0`,
		`SELECT 1 % 0`,
	)
}

// TestNumericColumns covers a declared precision and scale doing something.
func TestNumericColumns(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE prices (id INT PRIMARY KEY, amount NUMERIC(10,2), rate NUMERIC(12,6))`,
		`INSERT INTO prices VALUES (1, 9.5, 0.1), (2, 10, 1), (3, 0.005, 2.0000004)`,
	)
	p.checkAll(t,
		`SELECT id, amount, rate FROM prices ORDER BY id`,
		`SELECT sum(amount), avg(amount), min(amount), max(amount) FROM prices`,
		`SELECT amount * 2, amount / 3, amount + rate FROM prices ORDER BY id`,
		// A value past the declared precision is refused by both, with the same
		// code.
		`INSERT INTO prices VALUES (4, 999999999.00, 1) RETURNING id`,
		// Comparing a decimal against an integer, and grouping spellings of one
		// value together.
		`SELECT count(*) FROM prices WHERE amount = 10`,
		`SELECT count(*) FROM prices WHERE rate > 0.05`,
	)
}

// TestNumericAggregates covers the arithmetic people reach for DECIMAL to
// escape: totalling a column of money.
func TestNumericAggregates(t *testing.T) {
	p := open(t)
	p.setup(t,
		`CREATE TABLE items (id INT PRIMARY KEY, price NUMERIC(10,2), n INT)`,
		`INSERT INTO items VALUES (1, 0.10, 10), (2, 0.10, 11), (3, 0.10, 12), (4, NULL, NULL)`,
	)
	p.checkAll(t,
		`SELECT sum(price) FROM items`,
		`SELECT avg(price) FROM items`,
		// avg over integers is numeric in PostgreSQL, which is where the
		// sixteen places come from.
		`SELECT avg(n) FROM items`,
		`SELECT sum(n) FROM items`,
		`SELECT count(price), count(*) FROM items`,
		`SELECT price, count(*) FROM items GROUP BY price ORDER BY price`,
	)
}
