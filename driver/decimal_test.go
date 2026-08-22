package driver_test

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// TestDecimalIsExact is the whole point. Every one of these is a sum a float
// gets wrong, and getting them wrong is why a schema declares NUMERIC.
func TestDecimalIsExact(t *testing.T) {
	db := open(t)

	tests := []struct {
		name  string
		query string
		want  string
	}{
		// The canonical example. As float8 this is 0.30000000000000004.
		{"the addition floats cannot do", `SELECT 0.1 + 0.2`, "0.3"},
		// Ten cents, a hundred times over, is ten euros and not 9.99999...
		{"repeated addition does not drift",
			`SELECT 0.1 + 0.1 + 0.1 + 0.1 + 0.1 + 0.1 + 0.1 + 0.1 + 0.1 + 0.1`, "1.0"},
		{"scales add on multiplication", `SELECT 1.50 * 1.50`, "2.2500"},
		{"an integer joins the exact side", `SELECT 19.99 * 3`, "59.97"},
		{"subtraction to the penny", `SELECT 100.00 - 99.99`, "0.01"},
		// Well past what a float64 can represent at all.
		{"beyond float precision",
			`SELECT 9007199254740993 + 0.000000000000001`, "9007199254740993.000000000000001"},
		{"very large multiplication",
			`SELECT 123456789012345678901234567890 * 2`, "246913578024691357802469135780"},
		// Division picks PostgreSQL's scale rather than a fixed one.
		{"division of one by three", `SELECT 1 / 3.0`, "0.33333333333333333333"},
		{"division keeping sixteen significant digits", `SELECT 10 / 3.0`, "3.3333333333333333"},
		{"exact division still carries the scale", `SELECT 7.5 / 2.5`, "3.0000000000000000"},
		{"modulo follows the dividend", `SELECT 7.5 % 2`, "1.5"},
		{"negation", `SELECT -1.25`, "-1.25"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rowsOf(t, db, tt.query); !slices.Equal(got, []string{tt.want}) {
				t.Errorf("%s = %v, want [%s]", tt.query, got, tt.want)
			}
		})
	}
}

// TestDecimalLiteralIsNotFloat pins the rule that makes the above work: a
// literal with a point is numeric, as it is in PostgreSQL. Were it float, every
// expression it took part in would be inexact no matter what the columns said.
func TestDecimalLiteralIsNotFloat(t *testing.T) {
	db := open(t)

	var typeName string
	rows, err := db.Query(`SELECT 0.1`)
	if err != nil {
		t.Fatal(err)
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	typeName = types[0].DatabaseTypeName()
	rows.Close()

	if !strings.EqualFold(typeName, "numeric") {
		t.Errorf("a literal 0.1 has type %s, want numeric", typeName)
	}

	// Asking for the inexact kind explicitly still gets it, and behaves like it.
	if got := rowsOf(t, db, `SELECT CAST(0.1 AS DOUBLE PRECISION) + CAST(0.2 AS DOUBLE PRECISION)`); !slices.Equal(got, []string{"0.30000000000000004"}) {
		t.Errorf("float addition = %v, want the usual float answer", got)
	}
}

// TestDecimalColumnKeepsItsScale covers the declaration doing something: a
// column declared to two places stores two places, so what comes back is what
// the schema promised rather than whatever happened to be written.
func TestDecimalColumnKeepsItsScale(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE prices (id INT PRIMARY KEY, amount NUMERIC(10,2), rate NUMERIC(12,6))`,
		`INSERT INTO prices VALUES (1, 9.5, 0.1), (2, 10, 1), (3, 0.005, 2.0000004)`,
	)

	got := rowsOf(t, db, `SELECT amount, rate FROM prices ORDER BY id`)
	want := []string{
		"9.50|0.100000",
		"10.00|1.000000",
		// Rounded to the declared scale, half away from zero.
		"0.01|2.000000",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// A value too large for the declared precision is refused rather than
	// stored at a width the column said it would not hold.
	err := queryErr(db, `INSERT INTO prices VALUES (4, 999999999.00, 1)`)
	if err == nil {
		t.Fatal("a value past the declared precision was accepted")
	}
	if !strings.Contains(err.Error(), "numeric field overflow") {
		t.Errorf("got %v, want a numeric field overflow", err)
	}
}

// TestDecimalAggregatesStayExact covers the arithmetic people actually reach
// for DECIMAL to escape: totalling a column of money.
func TestDecimalAggregatesStayExact(t *testing.T) {
	db := open(t)
	mustExecAll(t, db, `CREATE TABLE items (id INT PRIMARY KEY, price NUMERIC(10,2))`)

	// A hundred rows of ten cents. Through float64 the total is 9.99999999999998.
	var b strings.Builder
	b.WriteString(`INSERT INTO items VALUES `)
	for i := range 100 {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`(`)
		b.WriteString(itoa(i))
		b.WriteString(`, 0.10)`)
	}
	mustExecAll(t, db, b.String())

	if got := rowsOf(t, db, `SELECT sum(price) FROM items`); !slices.Equal(got, []string{"10.00"}) {
		t.Errorf("sum = %v, want [10.00]", got)
	}
	if got := rowsOf(t, db, `SELECT avg(price) FROM items`); !slices.Equal(got, []string{"0.10000000000000000000"}) {
		t.Errorf("avg = %v, want an exact tenth", got)
	}
	// min and max keep the column's own type rather than promoting.
	if got := rowsOf(t, db, `SELECT min(price), max(price) FROM items`); !slices.Equal(got, []string{"0.10|0.10"}) {
		t.Errorf("min and max = %v", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestDecimalScansExactly pins the driver boundary. driver.Value has no exact
// decimal, so a numeric comes back as text -- which means a caller that wants
// every digit can have them, while one scanning into a float64 still works.
func TestDecimalScansExactly(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE t (id INT PRIMARY KEY, v NUMERIC(30,10))`,
		`INSERT INTO t VALUES (1, 12345678901234567890.1234567890)`,
	)

	var exact string
	if err := db.QueryRow(`SELECT v FROM t`).Scan(&exact); err != nil {
		t.Fatalf("scanning into a string: %v", err)
	}
	if exact != "12345678901234567890.1234567890" {
		t.Errorf("scanned %q, want every digit", exact)
	}

	// Scanning into a float64 still works, and rounds -- which is the caller's
	// choice rather than something the engine did on their behalf.
	var approx float64
	if err := db.QueryRow(`SELECT v FROM t`).Scan(&approx); err != nil {
		t.Errorf("scanning into a float64: %v", err)
	}

	// And a NULL is still a NULL.
	mustExecAll(t, db, `INSERT INTO t VALUES (2, NULL)`)
	var maybe sql.NullString
	if err := db.QueryRow(`SELECT v FROM t WHERE id = 2`).Scan(&maybe); err != nil {
		t.Fatalf("scanning a null: %v", err)
	}
	if maybe.Valid {
		t.Errorf("a NULL numeric scanned as %q", maybe.String)
	}
}

// TestDecimalComparesAndGroups covers the two places a new numeric kind does
// damage if Compare and Hash disagree: a WHERE that matches the wrong rows, and
// a GROUP BY that splits one value across two groups.
func TestDecimalComparesAndGroups(t *testing.T) {
	db := open(t)
	mustExecAll(t, db,
		`CREATE TABLE t (id INT PRIMARY KEY, v NUMERIC(10,3), n INT)`,
		// The same number written three ways, plus one that only differs
		// beyond what a float64 could tell apart.
		`INSERT INTO t VALUES (1, 1.5, 1), (2, 1.500, 1), (3, 1.50, 1), (4, 2.0, 2)`,
	)

	if got := rowsOf(t, db, `SELECT count(*) FROM t WHERE v = 1.5`); !slices.Equal(got, []string{"3"}) {
		t.Errorf("three spellings of 1.5 matched %v rows, want 3", got)
	}
	// Comparing a decimal column against an integer stays exact.
	if got := rowsOf(t, db, `SELECT count(*) FROM t WHERE v = 2`); !slices.Equal(got, []string{"1"}) {
		t.Errorf("2.0 = 2 matched %v, want 1", got)
	}

	// One group for the three spellings, not three.
	got := rowsOf(t, db, `SELECT v, count(*) FROM t GROUP BY v ORDER BY v`)
	if want := []string{"1.500|3", "2.000|1"}; !slices.Equal(got, want) {
		t.Errorf("groups = %v, want %v", got, want)
	}
}

// TestDecimalSurvivesARestart covers the log carrying every digit, including a
// value far too large to have gone through a float on the way.
func TestDecimalSurvivesARestart(t *testing.T) {
	dir := t.TempDir() + "/demo.db"

	db, shut := onDisk(t, dir)
	mustExec(t, db, `CREATE TABLE t (id INT PRIMARY KEY, v NUMERIC(40,10))`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 123456789012345678901234567890.1234567890), (2, 0.0000000001)`)
	shut()

	again, _ := onDisk(t, dir)
	assertRows(t, scanStrings(t, again, `SELECT v FROM t ORDER BY id`), []string{
		"123456789012345678901234567890.1234567890",
		"0.0000000001",
	})
}
