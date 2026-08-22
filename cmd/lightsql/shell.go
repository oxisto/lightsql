package main

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oxisto/lightsql/internal/ast"
	"github.com/oxisto/lightsql/internal/parser"
	"github.com/oxisto/lightsql/internal/pgerr"
	"github.com/oxisto/lightsql/internal/scanner"
	"github.com/oxisto/lightsql/internal/token"
)

// shell holds the state a session accumulates: what to print, where, and
// whether there is a transaction open.
type shell struct {
	db  *sql.DB
	out io.Writer
	err io.Writer
	dsn string

	format format
	timing bool
	// tx is the open transaction, or nil. The shell tracks one because the
	// engine has no BEGIN statement -- see beginTx.
	tx *sql.Tx
}

// repl reads statements and runs them, prompting when the input is a terminal.
//
// A script and a session go through the same loop rather than two, so a dot
// command works in both -- `printf '.tables\n' | lightsql ./db` is a reasonable
// thing to want, and a second code path for it would be a second place for
// statement splitting to be subtly different.
//
// The one thing that does differ is what an error means. A session prints it
// and carries on, because the next statement is usually a correction. A script
// stops, because the statements after a failed one usually depended on it, and
// running them produces a cascade whose first line was the only one worth
// reading.
func (s *shell) repl(in io.Reader, prompting bool) error {
	if prompting {
		s.printf("lightsql shell — %s\nType .help for commands, .quit to leave.\n\n", s.describe())
	}

	rd := bufio.NewScanner(in)
	rd.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var pending strings.Builder
	for {
		if prompting {
			s.print(s.prompt(pending.Len() > 0))
		}
		if !rd.Scan() {
			if prompting {
				s.print("\n")
			}
			break
		}
		line := rd.Text()

		// A dot command is only a dot command at the start of a statement.
		// Inside one it is ordinary text, which matters for a string literal
		// spanning lines.
		if pending.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), ".") {
			done, err := s.dot(strings.TrimSpace(line))
			if err != nil {
				if !prompting {
					return err
				}
				s.warnf("%v\n", err)
			}
			if done {
				return s.finish()
			}
			continue
		}

		pending.WriteString(line)
		pending.WriteString("\n")

		complete, err := statementComplete(pending.String())
		if err != nil {
			// The text cannot be scanned at all, so waiting for more of it
			// would wait forever.
			pending.Reset()
			if !prompting {
				return err
			}
			s.warnf("%v\n", err)
			continue
		}
		if !complete {
			continue
		}

		err = s.runScript(pending.String())
		pending.Reset()
		if err != nil {
			if !prompting {
				return err
			}
			s.warnf("%v\n", err)
		}
	}

	if err := rd.Err(); err != nil {
		return err
	}

	// Input can end without a final semicolon. In a script that is the last
	// statement -- `lightsql -c 'SELECT 1'` should not have to be punctuated --
	// so it is run, and any complaint about it comes from the parser, which
	// can say what is actually wrong with it.
	//
	// A session is different: input ending mid-statement is Ctrl-D partway
	// through typing one, and running half of it is the last thing the typist
	// wanted.
	if rest := strings.TrimSpace(pending.String()); rest != "" {
		if prompting {
			s.warn("discarding the unfinished statement")
			return s.finish()
		}
		if err := s.runScript(rest); err != nil {
			return err
		}
	}
	return s.finish()
}

// finish closes an open transaction rather than leaving it to the connection
// pool. Leaving without saying so would roll it back anyway; doing it here
// means the user is told.
func (s *shell) finish() error {
	if s.tx == nil {
		return nil
	}
	s.warn("rolling back the open transaction")
	err := s.tx.Rollback()
	s.tx = nil
	return err
}

func (s *shell) prompt(continued bool) string {
	if continued {
		return "     ...> "
	}
	if s.tx != nil {
		return "lightsql*> "
	}
	return "lightsql> "
}

func (s *shell) describe() string {
	if strings.HasPrefix(s.dsn, "file:") {
		return strings.TrimPrefix(s.dsn, "file:")
	}
	return "in memory (nothing is saved)"
}

// runScript executes every statement in src, stopping at the first failure.
//
// Stopping matters for a script: the statements after a failed one usually
// depend on it, and running them produces a cascade of errors whose first line
// is the only one worth reading.
func (s *shell) runScript(src string) error {
	stmts, err := parser.ParseAll(src)
	if err != nil {
		if h := hint(src, err); h != "" {
			// A syntax error on BEGIN is technically right and completely
			// unhelpful: the reader has typed valid SQL that this engine takes
			// through the driver instead. Say where to find it.
			return fmt.Errorf("%w\n%s", err, h)
		}
		return err
	}
	for _, st := range stmts {
		if err := s.runOne(st); err != nil {
			return err
		}
	}
	return nil
}

func (s *shell) runOne(st parser.Statement) error {
	start := time.Now()
	defer func() {
		if s.timing {
			s.printf("Time: %.3f ms\n", float64(time.Since(start).Microseconds())/1000)
		}
	}()

	if returnsRows(st.Stmt) {
		rows, err := s.querier().Query(st.Text)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		n, err := s.format.render(s.out, rows)
		if err != nil {
			return err
		}
		if s.format.countsRows() {
			s.printf("(%s)\n", plural(n, "row"))
		}
		return nil
	}

	res, err := s.querier().Exec(st.Text)
	if err != nil {
		return err
	}
	tag := strings.ToUpper(verbOf(st.Stmt))
	// A schema statement affects no rows, so saying "0 rows" after it invites
	// the reader to wonder which rows it missed. psql prints the tag alone.
	if changesRows(st.Stmt) {
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		tag += " " + plural(int(n), "row")
	}
	s.println(tag)
	return nil
}

// changesRows reports whether a row count means anything for this statement.
func changesRows(st ast.Stmt) bool {
	switch st.(type) {
	case *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt:
		return true
	default:
		return false
	}
}

// querier hides whether statements are going through a transaction, so nothing
// above has to branch on it.
type querier interface {
	Query(string, ...any) (*sql.Rows, error)
	Exec(string, ...any) (sql.Result, error)
}

func (s *shell) querier() querier {
	if s.tx != nil {
		return s.tx
	}
	return s.db
}

// returnsRows reports whether a statement produces a result set.
//
// It is decided from the parsed statement rather than by looking at the first
// word, because `INSERT ... RETURNING` returns rows and `INSERT` does not, and
// the difference is at the other end of the statement.
func returnsRows(st ast.Stmt) bool {
	switch st := st.(type) {
	case *ast.SelectStmt:
		return true
	case *ast.InsertStmt:
		return len(st.Returning) > 0
	case *ast.UpdateStmt:
		return len(st.Returning) > 0
	case *ast.DeleteStmt:
		return len(st.Returning) > 0
	default:
		return false
	}
}

// verbOf names a statement for the line printed after it runs, the way psql
// echoes INSERT or UPDATE back.
func verbOf(st ast.Stmt) string {
	switch st.(type) {
	case *ast.InsertStmt:
		return "insert"
	case *ast.UpdateStmt:
		return "update"
	case *ast.DeleteStmt:
		return "delete"
	case *ast.CreateTableStmt:
		return "create table"
	case *ast.DropTableStmt:
		return "drop table"
	case *ast.AlterTableStmt:
		return "alter table"
	case *ast.CreateIndexStmt:
		return "create index"
	case *ast.DropIndexStmt:
		return "drop index"
	default:
		return "ok"
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// printf and friends write the shell's own output.
//
// A write error on stdout is not actionable: the place a message about it would
// go is the thing that just failed. Dropping it in one place beats threading it
// through every call site, or checking it and doing nothing.
func (s *shell) printf(format string, args ...any) { _, _ = fmt.Fprintf(s.out, format, args...) }
func (s *shell) print(text string)                 { _, _ = fmt.Fprint(s.out, text) }
func (s *shell) println(text string)               { _, _ = fmt.Fprintln(s.out, text) }
func (s *shell) warnf(format string, args ...any)  { _, _ = fmt.Fprintf(s.err, format, args...) }
func (s *shell) warn(text string)                  { _, _ = fmt.Fprintln(s.err, text) }

// statementComplete reports whether src holds at least one whole statement, so
// the shell knows whether to run it or ask for another line.
//
// It uses the real scanner rather than looking for a semicolon, because a
// semicolon inside a string literal or a comment does not end anything --
// `INSERT INTO t VALUES ('a;b')` would otherwise be cut in half and both halves
// would fail to parse.
func statementComplete(src string) (bool, error) {
	toks, err := scanner.Tokens(src)
	if err != nil {
		// An unterminated literal or block comment is not an error yet: the
		// rest of it may be on the next line. Anything else is.
		if incomplete(err) {
			return false, nil
		}
		return false, err
	}
	// Tokens always ends with EOF, so the statement-ending semicolon is the one
	// before it.
	for i := len(toks) - 1; i >= 0; i-- {
		if toks[i].Kind == token.EOF {
			continue
		}
		return toks[i].Kind == token.Semicolon, nil
	}
	return false, nil
}

// incomplete reports whether a scan error means "there is more to come" rather
// than "this is wrong".
func incomplete(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "unterminated") || strings.Contains(msg, "unclosed")
}

// dot runs a shell command, reporting whether the shell should exit.
func (s *shell) dot(line string) (done bool, err error) {
	name, rest, _ := strings.Cut(line, " ")
	arg := strings.TrimSpace(rest)

	switch name {
	case ".help":
		s.print(helpText)
	case ".quit", ".exit":
		return true, nil
	case ".tables":
		return false, s.runScript(tablesQuery)
	case ".schema":
		return false, s.schema(arg)
	case ".mode":
		f, err := parseFormat(arg)
		if err != nil {
			return false, err
		}
		s.format = f
	case ".timer":
		switch arg {
		case "on":
			s.timing = true
		case "off":
			s.timing = false
		default:
			return false, errors.New(".timer takes on or off")
		}
	case ".begin":
		return false, s.beginTx()
	case ".commit":
		return false, s.endTx(true)
	case ".rollback":
		return false, s.endTx(false)
	default:
		return false, fmt.Errorf("unknown command %q; .help lists them", name)
	}
	return false, nil
}

const helpText = `.help              show this
.tables            list the tables in the public schema
.schema [table]    show the columns of one table, or of all of them
.mode FORMAT       output as table, csv or json
.timer on|off      report how long each statement took
.begin             start a transaction
.commit            commit it
.rollback          discard it
.quit              leave

Transactions are shell commands rather than SQL because lightsql has no BEGIN
statement: a transaction is taken through the driver, which is what lets it
honour the isolation level asked for instead of parsing one out of a statement.
`

const tablesQuery = `SELECT table_schema, table_name
	FROM information_schema.tables
	WHERE table_schema NOT IN ('information_schema', 'pg_catalog')
	ORDER BY table_schema, table_name`

// schema prints what a table is made of, from the catalog views rather than
// from a stored CREATE TABLE -- lightsql keeps no such text, and reconstructing
// one that differed from what was written would be worse than showing the
// pieces.
func (s *shell) schema(table string) error {
	query := `SELECT table_name, column_name, data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema NOT IN ('information_schema', 'pg_catalog')`
	if table != "" {
		query += fmt.Sprintf(" AND table_name = '%s'", strings.ReplaceAll(table, "'", "''"))
	}
	query += " ORDER BY table_name, ordinal_position"

	if err := s.runScript(query); err != nil {
		return err
	}

	constraints := `SELECT table_name, constraint_name, constraint_type
		FROM information_schema.table_constraints
		WHERE table_schema NOT IN ('information_schema', 'pg_catalog')`
	if table != "" {
		constraints += fmt.Sprintf(" AND table_name = '%s'", strings.ReplaceAll(table, "'", "''"))
	}
	return s.runScript(constraints + " ORDER BY table_name, constraint_type")
}

// beginTx opens a transaction.
//
// lightsql has no BEGIN statement, and that is deliberate rather than missing:
// a transaction is taken through the driver, which is what lets it honour the
// isolation level and read-only flag a caller asked for rather than parsing
// them out of a statement. A shell still needs the verb, so it provides it.
func (s *shell) beginTx() error {
	if s.tx != nil {
		return errors.New("a transaction is already open")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	s.tx = tx
	return nil
}

func (s *shell) endTx(commit bool) error {
	if s.tx == nil {
		return errors.New("no transaction is open")
	}
	tx := s.tx
	s.tx = nil
	if commit {
		return tx.Commit()
	}
	return tx.Rollback()
}

// sqlTransactionVerbs are the statements a user will reach for out of habit.
// They are not SQL here, so the shell says where to find them rather than
// letting the parser report a syntax error the reader cannot act on.
var sqlTransactionVerbs = []string{"begin", "commit", "rollback", "start"}

// hint turns a parse error on a transaction verb into the shell command that
// does what was meant.
//
// It reads the word the error points at rather than the first word of the
// script. A batch that ends in COMMIT fails at the COMMIT, and a hint derived
// from the CREATE TABLE that opened it would be about the wrong statement --
// which is worse than no hint, because it sends the reader to the wrong line.
func hint(src string, err error) string {
	var e *pgerr.Error
	if !errors.As(err, &e) || e.Pos < 0 || int(e.Pos) >= len(src) {
		return ""
	}
	word := strings.ToLower(wordAt(src, int(e.Pos)))
	if !slices.Contains(sqlTransactionVerbs, word) {
		return ""
	}
	if word == "start" {
		word = "begin"
	}
	return "transactions are shell commands here: try ." + word
}

// wordAt returns the run of letters beginning at off.
func wordAt(src string, off int) string {
	end := off
	for end < len(src) && (src[end] >= 'a' && src[end] <= 'z' || src[end] >= 'A' && src[end] <= 'Z') {
		end++
	}
	return src[off:end]
}
