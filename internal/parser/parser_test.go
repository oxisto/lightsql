package parser

import (
	"errors"
	"strings"
	"testing"

	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/token"
)

// parseExprString parses a bare expression by wrapping it in a trivial SELECT,
// so that precedence cases stay readable.
func parseExprString(t *testing.T, expr string) ast.Expr {
	t.Helper()
	stmt, err := ParseOne("SELECT " + expr)
	if err != nil {
		t.Fatalf("parsing %q: %v", expr, err)
	}
	sel, ok := stmt.(*ast.SelectStmt)
	if !ok {
		t.Fatalf("parsing %q: got %T, want *ast.SelectStmt", expr, stmt)
	}
	if len(sel.Items) != 1 {
		t.Fatalf("parsing %q: got %d select items, want 1", expr, len(sel.Items))
	}
	return sel.Items[0].Expr
}

// flat renders a tree on one line, so expectations can sit inline in the table.
func flat(n ast.Node) string {
	return strings.Join(strings.Fields(ast.Sprint(n)), " ")
}

// TestExprPrecedence is the core parser test. It pins operator precedence and
// associativity by asserting on the whole tree, which is the only way these
// mistakes are visible: a parser that drops an operand or flattens AND and OR
// into a sibling list still "parses" every one of these inputs.
func TestExprPrecedence(t *testing.T) {
	tests := []struct {
		expr string
		want string
	}{
		// The regression that motivates the whole design: an ad hoc parser that
		// checks for one operator and stops silently drops "+ c" here.
		{"a * b + c", "(+ (* (col a) (col b)) (col c))"},
		{"a + b * c", "(+ (col a) (* (col b) (col c)))"},

		// Subtraction and division are left associative, so a - b - c is
		// (a - b) - c and not a - (b - c).
		{"a - b - c", "(- (- (col a) (col b)) (col c))"},
		{"a / b / c", "(/ (/ (col a) (col b)) (col c))"},
		{"a || b || c", "(|| (|| (col a) (col b)) (col c))"},

		// The JSON operators are left associative, so a chained lookup walks
		// into the document rather than trying to index the key.
		{"a -> 'x' -> 'y'", `(-> (-> (col a) (lit "x")) (lit "y"))`},
		{"a -> 'x' ->> 'y'", `(->> (-> (col a) (lit "x")) (lit "y"))`},

		// They bind tighter than comparison, which is what makes the common
		// predicate doc ->> 'k' = 'v' mean what it looks like. Written the
		// other way round it would compare a document against a boolean.
		{"a ->> 'k' = 'v'", `(= (->> (col a) (lit "k")) (lit "v"))`},
		{"a @> b = c", "(= (@> (col a) (col b)) (col c))"},

		// And tighter than concatenation, so the arrow wins over the pipes.
		{"a ->> 'k' || 'x'", `(|| (->> (col a) (lit "k")) (lit "x"))`},

		// A datetime value function is a keyword standing for a value, so it
		// composes like any other operand rather than needing a call.
		{"current_timestamp", "(current_timestamp)"},
		{"current_date", "(current_date)"},
		{"current_time", "(current_time)"},
		{"localtimestamp", "(localtimestamp)"},
		{"localtime", "(localtime)"},
		{"ts < current_timestamp", "(< (col ts) (current_timestamp))"},
		{"current_date = d", "(= (current_date) (col d))"},

		// A minus directly against an identifier must not be folded into a
		// negative literal by the scanner.
		{"a-1", "(- (col a) (lit 1))"},
		{"a - 1", "(- (col a) (lit 1))"},

		// AND binds tighter than OR, so the tree must nest rather than list.
		{"a OR b AND c", "(or (col a) (and (col b) (col c)))"},
		{"a AND b OR c", "(or (and (col a) (col b)) (col c))"},
		{"a OR b OR c", "(or (or (col a) (col b)) (col c))"},

		// Parentheses override precedence, and are not preserved in the tree.
		{"(a OR b) AND c", "(and (or (col a) (col b)) (col c))"},

		// Comparison binds tighter than AND, which binds tighter than OR.
		{
			"a = 1 OR b = 2 AND c = 3",
			"(or (= (col a) (lit 1)) (and (= (col b) (lit 2)) (= (col c) (lit 3))))",
		},

		// NOT sits between AND and comparison: it takes a whole comparison as
		// its operand but does not reach across AND.
		{"NOT a = b", "(not (= (col a) (col b)))"},
		{"NOT a AND b", "(and (not (col a)) (col b))"},
		{"NOT NOT a", "(not (not (col a)))"},

		// PostgreSQL makes exponentiation left associative, and binds unary
		// minus tighter than it, so -2^2 is 4 rather than -4.
		{"2 ^ 3 ^ 2", "(^ (^ (lit 2) (lit 3)) (lit 2))"},
		{"-2 ^ 2", "(^ (neg (lit 2)) (lit 2))"},
		{"- a * b", "(* (neg (col a)) (col b))"},

		// A cast binds tighter than any arithmetic.
		{"a::int + 1", "(+ (cast (type int) (col a)) (lit 1))"},
		{"CAST(a AS int) + 1", "(+ (cast (type int) (col a)) (lit 1))"},

		// IS NULL is a distinct node, because unlike = NULL it yields a
		// definite true or false.
		{"a IS NULL", "(is-null (col a))"},
		{"a IS NOT NULL", "(is-not-null (col a))"},
		{
			"a IS NULL AND b IS NOT NULL",
			"(and (is-null (col a)) (is-not-null (col b)))",
		},
		{"a IS DISTINCT FROM b", "(is-distinct-from (col a) (col b))"},
		{"a IS NOT DISTINCT FROM b", "(is-not-distinct-from (col a) (col b))"},
		// IS TRUE is exactly IS NOT DISTINCT FROM TRUE, so it desugars rather
		// than carrying its own node.
		{"a IS TRUE", "(is-not-distinct-from (col a) (lit true))"},
		{"a IS NOT FALSE", "(is-distinct-from (col a) (lit false))"},

		// The AND inside BETWEEN belongs to BETWEEN, not to the surrounding
		// boolean expression.
		{"a BETWEEN 1 AND 2", "(between (col a) (lit 1) (lit 2))"},
		{
			"a BETWEEN 1 AND 2 AND b",
			"(and (between (col a) (lit 1) (lit 2)) (col b))",
		},
		{"a NOT BETWEEN 1 AND 2", "(not-between (col a) (lit 1) (lit 2))"},

		// NOT in infix position belongs to the operator that follows it.
		{"a IN (1, 2)", "(in (col a) (list (lit 1) (lit 2)))"},
		{"a NOT IN (1, 2)", "(not-in (col a) (list (lit 1) (lit 2)))"},
		{"a LIKE 'x%'", `(like (col a) (lit "x%"))`},
		{"a NOT LIKE 'x%'", `(not-like (col a) (lit "x%"))`},

		// Qualified references keep all their parts.
		{"t.c", "(col t.c)"},
		{"s.t.c", "(col s.t.c)"},
		{"count(*)", "(call count *)"},
		{"count(DISTINCT a)", "(call count distinct (col a))"},
		{"coalesce(a, b, 0)", "(call coalesce (col a) (col b) (lit 0))"},

		// Placeholders keep the ordinal the scanner assigned.
		{"$2 + $1", "(+ (param 2) (param 1))"},
	}

	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			if got := flat(parseExprString(t, tt.expr)); got != tt.want {
				t.Errorf("parsing %q\n got: %s\nwant: %s", tt.expr, got, tt.want)
			}
		})
	}
}

func TestParseCase(t *testing.T) {
	got := flat(parseExprString(t, "CASE WHEN a > 1 THEN 'big' ELSE 'small' END"))
	want := `(case (when (> (col a) (lit 1)) (lit "big")) (else (lit "small")))`
	if got != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}

	got = flat(parseExprString(t, "CASE a WHEN 1 THEN 'one' END"))
	want = `(case (operand (col a)) (when (lit 1) (lit "one")))`
	if got != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}
}

func TestParseStatements(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "select star",
			sql:  "SELECT * FROM users",
			want: "(select (items (star)) (from (table users)))",
		},
		{
			name: "qualified star",
			sql:  "SELECT u.* FROM users u",
			want: "(select (items (star u)) (from (table users as u)))",
		},
		{
			name: "aliases with and without AS",
			sql:  "SELECT a AS x, b y FROM t",
			want: "(select (items (as x (col a)) (as y (col b))) (from (table t)))",
		},
		{
			name: "quoted identifiers keep their case",
			sql:  `SELECT "Name" FROM "User"`,
			want: `(select (items (col Name)) (from (table User)))`,
		},
		{
			// CROSS and USING have to be keywords, or the alias rule would
			// swallow them as a table alias.
			name: "cross join",
			sql:  "SELECT 1 FROM a CROSS JOIN b",
			want: "(select (items (lit 1)) (from (join cross (table a) (table b))))",
		},
		{
			name: "left outer join is a left join",
			sql:  "SELECT 1 FROM a LEFT OUTER JOIN b ON a.id = b.id",
			want: "(select (items (lit 1)) (from (join left (table a) (table b) " +
				"(on (= (col a.id) (col b.id))))))",
		},
		{
			name: "join using",
			sql:  "SELECT 1 FROM a JOIN b USING (id)",
			want: "(select (items (lit 1)) (from (join inner (table a) (table b) (using id))))",
		},
		{
			name: "joins nest left to right",
			sql:  "SELECT 1 FROM a JOIN b ON x JOIN c ON y",
			want: "(select (items (lit 1)) (from (join inner (join inner (table a) " +
				"(table b) (on (col x))) (table c) (on (col y)))))",
		},
		{
			name: "group by and having",
			sql:  "SELECT k, count(*) FROM t GROUP BY k HAVING count(*) > 1",
			want: "(select (items (col k) (call count *)) (from (table t)) " +
				"(group-by (col k)) (having (> (call count *) (lit 1))))",
		},
		{
			name: "order by with direction and nulls",
			sql:  "SELECT a FROM t ORDER BY a DESC NULLS FIRST, b",
			want: "(select (items (col a)) (from (table t)) " +
				"(order-by (term desc nulls-first (col a)) (term asc (col b))))",
		},
		{
			name: "limit and offset",
			sql:  "SELECT a FROM t LIMIT 10 OFFSET 5",
			want: "(select (items (col a)) (from (table t)) (limit (lit 10)) (offset (lit 5)))",
		},
		{
			name: "offset before limit",
			sql:  "SELECT a FROM t OFFSET 5 LIMIT 10",
			want: "(select (items (col a)) (from (table t)) (limit (lit 10)) (offset (lit 5)))",
		},
		{
			name: "distinct on",
			sql:  "SELECT DISTINCT ON (a) a, b FROM t",
			want: "(select (distinct-on (col a)) (items (col a) (col b)) (from (table t)))",
		},
		{
			name: "derived table",
			sql:  "SELECT x FROM (SELECT a AS x FROM t) s",
			want: "(select (items (col x)) (from (derived as s " +
				"(select (items (as x (col a))) (from (table t))))))",
		},
		{
			name: "scalar subquery",
			sql:  "SELECT (SELECT 1) FROM t",
			want: "(select (items (subquery (select (items (lit 1))))) (from (table t)))",
		},
		{
			name: "exists subquery",
			sql:  "SELECT 1 WHERE EXISTS (SELECT 1 FROM t)",
			want: "(select (items (lit 1)) (where (exists (select (items (lit 1)) (from (table t))))))",
		},
		{
			name: "in subquery",
			sql:  "SELECT 1 WHERE a IN (SELECT b FROM t)",
			want: "(select (items (lit 1)) (where (in (col a) " +
				"(select (items (col b)) (from (table t))))))",
		},
		{
			name: "insert with values",
			sql:  "INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y')",
			want: `(insert t (columns a b) (values (lit 1) (lit "x")) (values (lit 2) (lit "y")))`,
		},
		{
			name: "insert returning",
			sql:  "INSERT INTO t (a) VALUES ($1) RETURNING id",
			want: "(insert t (columns a) (values (param 1)) (returning (col id)))",
		},
		{
			name: "insert from select",
			sql:  "INSERT INTO t (a) SELECT b FROM u",
			want: "(insert t (columns a) (select (select (items (col b)) (from (table u)))))",
		},
		{
			name: "schema qualified table",
			sql:  "SELECT 1 FROM public.users",
			want: "(select (items (lit 1)) (from (table public.users)))",
		},
		{
			name: "update",
			sql:  "UPDATE t SET a = 1, b = b + 1 WHERE c = $1",
			want: "(update t (set (= a (lit 1)) (= b (+ (col b) (lit 1)))) " +
				"(where (= (col c) (param 1))))",
		},
		{
			name: "update without where affects every row",
			sql:  "UPDATE t SET a = 1",
			want: "(update t (set (= a (lit 1))))",
		},
		{
			name: "update with alias and returning",
			sql:  "UPDATE t AS x SET a = 1 WHERE x.b = 2 RETURNING x.a AS n",
			want: "(update t as x (set (= a (lit 1))) (where (= (col x.b) (lit 2))) " +
				"(returning (as n (col x.a))))",
		},
		{
			name: "delete",
			sql:  "DELETE FROM t WHERE a = 1",
			want: "(delete t (where (= (col a) (lit 1))))",
		},
		{
			name: "delete without where empties the table",
			sql:  "DELETE FROM t",
			want: "(delete t)",
		},
		{
			name: "delete returning star",
			sql:  "DELETE FROM t WHERE a = 1 RETURNING *",
			want: "(delete t (where (= (col a) (lit 1))) (returning (star)))",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt, err := ParseOne(tt.sql)
			if err != nil {
				t.Fatalf("ParseOne(%q): %v", tt.sql, err)
			}
			if got := flat(stmt); got != tt.want {
				t.Errorf("parsing %q\n got: %s\nwant: %s", tt.sql, got, tt.want)
			}
		})
	}
}

func TestParseCreateTable(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "drop table",
			sql:  "DROP TABLE t",
			want: "(drop-table t)",
		},
		{
			name: "insert on conflict do nothing",
			sql:  "INSERT INTO t (a) VALUES (1) ON CONFLICT DO NOTHING",
			want: "(insert t (columns a) (values (lit 1)) (on-conflict do-nothing))",
		},
		{
			name: "insert on conflict do update",
			sql:  "INSERT INTO t (a, b) VALUES (1, 2) ON CONFLICT (a) DO UPDATE SET b = excluded.b",
			want: "(insert t (columns a b) (values (lit 1) (lit 2)) " +
				"(on-conflict a (do-update (= b (col excluded.b)))))",
		},
		{
			name: "insert on conflict do update where",
			sql:  "INSERT INTO t (a) VALUES (1) ON CONFLICT (a) DO UPDATE SET b = 1 WHERE t.b < 5",
			want: "(insert t (columns a) (values (lit 1)) " +
				"(on-conflict a (do-update (= b (lit 1))) (where (< (col t.b) (lit 5)))))",
		},
		{
			name: "alter table add column",
			sql:  "ALTER TABLE t ADD COLUMN c INT NOT NULL DEFAULT 0",
			want: "(alter-table t (add-column (column c (type int) " +
				"(constraint not null) (constraint default (lit 0)))))",
		},
		{
			// COLUMN is optional here too.
			name: "alter table add without the keyword",
			sql:  "ALTER TABLE t ADD c INT",
			want: "(alter-table t (add-column (column c (type int))))",
		},
		{
			name: "alter table add column if not exists",
			sql:  "ALTER TABLE t ADD COLUMN IF NOT EXISTS c INT",
			want: "(alter-table t (add-column if-not-exists (column c (type int))))",
		},
		{
			name: "alter table rename to",
			sql:  "ALTER TABLE t RENAME TO u",
			want: "(alter-table t (rename-to u))",
		},
		{
			name: "alter table rename column",
			sql:  "ALTER TABLE t RENAME COLUMN a TO b",
			want: "(alter-table t (rename-column a b))",
		},
		{
			// COLUMN is optional, so this is still the column form.
			name: "alter table rename column without the keyword",
			sql:  "ALTER TABLE t RENAME a TO b",
			want: "(alter-table t (rename-column a b))",
		},
		{
			name: "generated by default as identity",
			sql:  "CREATE TABLE t (id INT GENERATED BY DEFAULT AS IDENTITY)",
			want: "(create-table t (column id (type int) (constraint generated by default as identity)))",
		},
		{
			name: "generated always as identity",
			sql:  "CREATE TABLE t (id INT PRIMARY KEY GENERATED ALWAYS AS IDENTITY)",
			want: "(create-table t (column id (type int) (constraint primary key) " +
				"(constraint generated always as identity)))",
		},
		{
			// generated, always and identity are unreserved, so they stay
			// usable as names. Reserving them would break the far more common
			// SELECT x AS identity.
			name: "identity is not a reserved word",
			sql:  "SELECT identity AS generated FROM always",
			want: "(select (items (as generated (col identity))) (from (table always)))",
		},
		{
			name: "alter column set not null",
			sql:  "ALTER TABLE t ALTER COLUMN a SET NOT NULL",
			want: "(alter-table t (set-not-null a))",
		},
		{
			name: "alter column drop not null",
			sql:  "ALTER TABLE t ALTER COLUMN a DROP NOT NULL",
			want: "(alter-table t (drop-not-null a))",
		},
		{
			// COLUMN is optional here as it is after RENAME.
			name: "alter column without the keyword",
			sql:  "ALTER TABLE t ALTER a SET NOT NULL",
			want: "(alter-table t (set-not-null a))",
		},
		{
			// alter is unreserved, so it stays usable as a column name.
			name: "alter is not a reserved word",
			sql:  "SELECT alter FROM t",
			want: "(select (items (col alter)) (from (table t)))",
		},
		{
			name: "create index",
			sql:  "CREATE INDEX i ON t (a, b)",
			want: "(create-index i t a b)",
		},
		{
			name: "create unique index if not exists",
			sql:  "CREATE UNIQUE INDEX IF NOT EXISTS i ON t (a)",
			want: "(create-index i t unique if-not-exists a)",
		},
		{
			name: "partial index",
			sql:  "CREATE UNIQUE INDEX i ON t (a) WHERE b > 1",
			want: "(create-index i t unique a (where (> (col b) (lit 1))))",
		},
		{
			// index is unreserved, so it stays usable as a column name.
			name: "index is not a reserved word",
			sql:  "SELECT index FROM t",
			want: "(select (items (col index)) (from (table t)))",
		},
		{
			name: "drop index",
			sql:  "DROP INDEX IF EXISTS i, j",
			want: "(drop-index i j if-exists)",
		},
		{
			name: "drop several tables if exists",
			sql:  "DROP TABLE IF EXISTS a, public.b",
			want: "(drop-table a public.b if-exists)",
		},
		{
			// RESTRICT is the default, so writing it changes nothing and it
			// does not appear in the tree.
			name: "drop table restrict",
			sql:  "DROP TABLE t RESTRICT",
			want: "(drop-table t)",
		},
		{
			name: "drop table cascade",
			sql:  "DROP TABLE t CASCADE",
			want: "(drop-table t cascade)",
		},
		{
			name: "column types and modifiers",
			sql:  "CREATE TABLE t (a INT, b VARCHAR(255), c NUMERIC(10, 2))",
			want: "(create-table t (column a (type int)) (column b (type varchar 255)) " +
				"(column c (type numeric 10 2)))",
		},
		{
			name: "multi word types",
			sql:  "CREATE TABLE t (a DOUBLE PRECISION, b CHARACTER VARYING(10), c CHARACTER)",
			want: "(create-table t (column a (type double precision)) " +
				"(column b (type character varying 10)) (column c (type char)))",
		},
		{
			name: "timestamp with time zone",
			sql:  "CREATE TABLE t (a TIMESTAMP WITH TIME ZONE, b TIMESTAMP WITHOUT TIME ZONE)",
			want: "(create-table t (column a (type timestamptz)) (column b (type timestamp)))",
		},
		{
			name: "column constraints",
			sql: "CREATE TABLE t (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL UNIQUE, " +
				"n INT DEFAULT 0 CHECK (n >= 0))",
			want: "(create-table t (column id (type bigserial) (constraint primary key)) " +
				"(column name (type text) (constraint not null) (constraint unique)) " +
				"(column n (type int) (constraint default (lit 0)) " +
				"(constraint check (>= (col n) (lit 0)))))",
		},
		{
			name: "if not exists",
			sql:  "CREATE TABLE IF NOT EXISTS t (a INT)",
			want: "(create-table t if-not-exists (column a (type int)))",
		},
		{
			name: "column level foreign key with actions",
			sql:  "CREATE TABLE t (a INT REFERENCES u (id) ON DELETE CASCADE ON UPDATE SET NULL)",
			want: "(create-table t (column a (type int) (constraint references " +
				"(references u id on-delete=cascade on-update=set-null))))",
		},
		{
			name: "table level constraints",
			sql: "CREATE TABLE t (a INT, b INT, PRIMARY KEY (a, b), " +
				"CONSTRAINT fk FOREIGN KEY (a) REFERENCES u (id))",
			want: "(create-table t (column a (type int)) (column b (type int)) " +
				"(table-constraint primary key a b) " +
				"(table-constraint references fk a (references u id)))",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt, err := ParseOne(tt.sql)
			if err != nil {
				t.Fatalf("ParseOne(%q): %v", tt.sql, err)
			}
			if got := flat(stmt); got != tt.want {
				t.Errorf("parsing %q\n got: %s\nwant: %s", tt.sql, got, tt.want)
			}
		})
	}
}

func TestParseBatch(t *testing.T) {
	stmts, err := Parse("CREATE TABLE t (a INT); INSERT INTO t (a) VALUES (1);")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want 2", len(stmts))
	}
	if _, err := ParseOne("SELECT 1; SELECT 2"); err == nil {
		t.Error("ParseOne accepted a batch, want an error")
	}
}

// TestParseErrors checks that every failure carries a SQLSTATE and a position
// that points at the offending token.
func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantPos token.Pos
		wantMsg string
	}{
		{"missing from target", "SELECT a FROM", 13, "end of input"},
		{"dangling operator", "SELECT a +", 10, "end of input"},
		{"unclosed paren", "SELECT (a", 9, "end of input"},
		{"junk after statement", "SELECT a b c", 11, `at or near "c"`},
		{"join without condition", "SELECT 1 FROM a JOIN b", 22, "ON or USING"},
		{"bad referential action", "CREATE TABLE t (a INT REFERENCES u ON DELETE MAYBE)", 45, "referential action"},
		{"empty table", "CREATE TABLE t ()", 16, `at or near ")"`},
		{"case without when", "SELECT CASE END", 12, "WHEN"},
		{"bad is", "SELECT a IS 1", 12, "NULL, TRUE, FALSE or DISTINCT FROM"},
		// A bare NOT in infix position is only valid before IN, LIKE or
		// BETWEEN, so the error names the NOT rather than what follows it.
		{"not alone", "SELECT a NOT b", 9, `at or near "NOT"`},
		{"update without set", "UPDATE t WHERE a = 1", 9, "SET"},
		{"update assignment without value", "UPDATE t SET a =", 16, "end of input"},
		{"delete without from", "DELETE t", 7, "FROM"},
		// An UPDATE alias must be introduced with AS, so a bare identifier here
		// is reported against the missing SET rather than silently accepted.
		{"update bare alias", "UPDATE t x SET a = 1", 9, "SET"},
		// The datetime value functions are reserved, so they cannot be an
		// alias -- which is the whole reason they are keywords rather than
		// identifiers matched by name.
		{"reserved as alias", "SELECT 1 AS current_timestamp", 12, `at or near "CURRENT_TIMESTAMP"`},
		{"identity without always or by default", "CREATE TABLE t (id INT GENERATED AS IDENTITY)", 33, "ALWAYS or BY DEFAULT"},
		{"identity without the keyword", "CREATE TABLE t (id INT GENERATED ALWAYS AS ROWS)", 43, `at or near "rows", expected identity`},
		{"alter column with no action", "ALTER TABLE t ALTER COLUMN a", 28, "SET NOT NULL or DROP NOT NULL"},
		{"alter column set without not null", "ALTER TABLE t ALTER a SET NULL", 26, "expected NOT"},
		// PostgreSQL accepts a precision, e.g. CURRENT_TIMESTAMP(0). lightsql
		// stores microseconds and has nothing to round to, so the form is
		// refused rather than silently ignored.
		{"precision is not accepted", "SELECT current_timestamp(0)", 24, `at or near "("`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.sql)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want an error", tt.sql)
			}
			var e *pgerr.Error
			if !errors.As(err, &e) {
				t.Fatalf("error %v is not a *pgerr.Error", err)
			}
			if e.Code != pgerr.SyntaxError {
				t.Errorf("SQLSTATE = %s, want %s", e.Code, pgerr.SyntaxError)
			}
			if !strings.Contains(e.Message, tt.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", e.Message, tt.wantMsg)
			}
			if e.Pos != tt.wantPos {
				t.Errorf("position = %d, want %d (message: %s)", e.Pos, tt.wantPos, e.Message)
			}
		})
	}
}

// TestParseRefusals covers the forms the parser understands and declines,
// which are a different thing from the ones it cannot read.
//
// They carry feature_not_supported rather than a syntax error, and they name
// the construct: a reader who writes DROP COLUMN has written valid SQL and is
// owed a reason, not a complaint about the token after it.
func TestParseRefusals(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantPos token.Pos
		wantMsg string
	}{
		{"drop column", "ALTER TABLE t DROP COLUMN a", 14,
			"DROP COLUMN is not supported"},
		{"alter column type", "ALTER TABLE t ALTER COLUMN a TYPE INT", 29,
			"TYPE is not supported"},
		{"alter column set default", "ALTER TABLE t ALTER COLUMN a SET DEFAULT 1", 33,
			"SET or DROP DEFAULT"},
		{"alter column drop default", "ALTER TABLE t ALTER COLUMN a DROP DEFAULT", 34,
			"SET or DROP DEFAULT"},
		{"identity sequence options", "CREATE TABLE t (id INT GENERATED ALWAYS AS IDENTITY (START WITH 100))", 52,
			"sequence options on an identity column"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.sql)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want a refusal", tt.sql)
			}
			var e *pgerr.Error
			if !errors.As(err, &e) {
				t.Fatalf("error %v is not a *pgerr.Error", err)
			}
			if e.Code != pgerr.FeatureNotSupported {
				t.Errorf("SQLSTATE = %s, want %s", e.Code, pgerr.FeatureNotSupported)
			}
			if !strings.Contains(e.Message, tt.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", e.Message, tt.wantMsg)
			}
			if e.Pos != tt.wantPos {
				t.Errorf("position = %d, want %d (message: %s)", e.Pos, tt.wantPos, e.Message)
			}
		})
	}
}

// TestParseIsRepeatable pins the invariant that the parser does not consume or
// rewrite its input, so the same text always yields the same tree. A parser
// whose executor mutates the AST as it walks cannot re-run a prepared statement.
func TestParseIsRepeatable(t *testing.T) {
	const sql = "SELECT a, b FROM t WHERE a > $1 AND b IS NOT NULL ORDER BY a DESC LIMIT 5"
	first, err := ParseOne(sql)
	if err != nil {
		t.Fatalf("ParseOne: %v", err)
	}
	before := ast.Sprint(first)

	second, err := ParseOne(sql)
	if err != nil {
		t.Fatalf("ParseOne: %v", err)
	}
	if got := ast.Sprint(second); got != before {
		t.Errorf("reparsing produced a different tree\n got: %s\nwant: %s", got, before)
	}
	// Printing the first tree again must also be unchanged, which would fail if
	// anything walked it destructively.
	if got := ast.Sprint(first); got != before {
		t.Errorf("re-printing changed the tree\n got: %s\nwant: %s", got, before)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"SELECT * FROM t WHERE a = $1",
		"INSERT INTO t (a, b) VALUES (1, 'x') RETURNING id",
		"CREATE TABLE t (id BIGSERIAL PRIMARY KEY, n INT REFERENCES u (id))",
		"SELECT a FROM t JOIN u ON t.id = u.id GROUP BY a HAVING count(*) > 1",
		"SELECT CASE WHEN a THEN 1 ELSE 2 END",
		"SELECT a BETWEEN 1 AND 2 AND b",
	} {
		f.Add(s)
	}
	// Parsing must never panic and never hang; malformed input must produce an
	// error rather than a partial tree.
	f.Fuzz(func(t *testing.T, src string) {
		_, _ = Parse(src)
	})
}

// TestParseAllKeepsStatementText covers what the write-ahead log records for a
// DDL statement. The text has to be the statement and nothing else: a trailing
// separator or a comment belonging to the next statement would be replayed as
// part of this one.
func TestParseAllKeepsStatementText(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "one statement, no separator",
			src:  "select 1",
			want: []string{"select 1"},
		},
		{
			name: "a trailing semicolon is not part of the statement",
			src:  "select 1;",
			want: []string{"select 1"},
		},
		{
			name: "a comment between statements belongs to neither",
			src:  "create table t (a int); -- why\n\ninsert into t values (1);",
			want: []string{"create table t (a int)", "insert into t values (1)"},
		},
		{
			name: "a semicolon inside a literal does not split",
			src:  "insert into t values ('a;b'); select 2",
			want: []string{"insert into t values ('a;b')", "select 2"},
		},
		{
			name: "empty statements between separators are skipped",
			src:  ";; select 1 ;; select 2 ;;",
			want: []string{"select 1", "select 2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAll(tt.src)
			if err != nil {
				t.Fatalf("ParseAll: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parsed %d statements, want %d", len(got), len(tt.want))
			}
			for i, want := range tt.want {
				if got[i].Text != want {
					t.Errorf("statement %d text = %q, want %q", i, got[i].Text, want)
				}
			}
		})
	}
}
