// Command lightsql is a shell for a lightsql database, in the shape of the
// sqlite3 and psql ones: open a database, type SQL at it, look at what comes
// back.
//
// It exists because an embedded database that can only be reached from inside
// the program that wrote it is hard to trust. Being able to open the directory
// afterwards and ask what is actually in there is the difference between
// believing the tests and knowing.
//
// Usage:
//
//	lightsql ./demo.db                 open a directory and start a shell
//	lightsql                           the same, in memory, discarded on exit
//	lightsql -c 'SELECT 1' ./demo.db   run one statement and exit
//	lightsql -f setup.sql ./demo.db    run a file
//	echo 'SELECT 1' | lightsql         read from a pipe
//
// It links only the standard library and lightsql itself, so `go install` of it
// pulls in nothing else -- the same promise the library makes.
package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	lightsqldriver "github.com/oxisto/lightsql/driver"
	"github.com/oxisto/lightsql/internal/engine"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "lightsql: %v\n", err)
		os.Exit(1)
	}
}

// run is separated from main so the tests can drive it with their own streams
// and check what it writes, rather than shelling out to a built binary.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("lightsql", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		command = fs.String("c", "", "run one statement (or several, separated by semicolons) and exit")
		file    = fs.String("f", "", "run the statements in a file and exit")
		mode    = fs.String("format", "table", "output format: table, csv or json")
		fsyncOn = fs.Bool("fsync", true, "flush each commit to disk; off is faster and loses the guarantee")
		history = fs.Bool("history", false, "list the schema statements in the log and exit")
		at      = fs.Int("at", 0, "open replaying only the first N schema statements, read-only")
	)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	format, err := parseFormat(*mode)
	if err != nil {
		return err
	}

	dsn, err := dataSource(fs.Args(), *fsyncOn)
	if err != nil {
		return err
	}

	if *history || *at > 0 {
		dir, ok := strings.CutPrefix(dsn, "file:")
		if !ok {
			return errors.New("-history and -at need a directory, not an in-memory name")
		}
		// The query string is a data source concern; these read the directory.
		dir, _, _ = strings.Cut(dir, "?")
		if *history {
			return printHistory(stdout, dir)
		}
		return recoverAt(dir, *at, stdin, stdout, stderr, format, *command, *file)
	}

	db, err := sql.Open("lightsql", dsn)
	if err != nil {
		return err
	}
	// Registered before the close below, so it runs after it: the engine
	// outlives the *sql.DB, since closing a database handle cannot tell the
	// driver that nothing will use the name again.
	//
	// For a directory this checkpoints the log, so the next start replays the
	// rows that survived rather than every change ever made. For an in-memory
	// database it is what makes a second invocation a second database, rather
	// than finding the tables the first one left in a process-wide registry.
	defer func() { lightsqldriver.Drop(dsn) }()
	defer func() { _ = db.Close() }()
	// sql.Open only parses, so nothing has touched the directory yet. Reaching
	// the engine here turns "the path is wrong" into an error now rather than
	// after the first statement is typed.
	if err := db.Ping(); err != nil {
		return err
	}

	sh := &shell{db: db, out: stdout, err: stderr, format: format, dsn: dsn}

	switch {
	case *command != "":
		return sh.repl(strings.NewReader(*command), false)
	case *file != "":
		src, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		return sh.repl(strings.NewReader(string(src)), false)
	default:
		// Piped input is a script with no prompts, so that lightsql composes
		// with the shell around it.
		return sh.repl(stdin, isTerminal(stdin))
	}
}

// printHistory lists the schema statements a log holds, numbered.
//
// The number is what -at takes. Someone recovering from a mistaken DROP TABLE
// reads this to find the statement that did it, and opens at the one before.
func printHistory(out io.Writer, dir string) error {
	stmts, err := engine.SchemaHistory(dir)
	if err != nil {
		return err
	}
	for i, s := range stmts {
		_, _ = fmt.Fprintf(out, "%3d  %s\n", i+1, strings.Join(strings.Fields(s), " "))
	}
	if len(stmts) == 0 {
		_, _ = fmt.Fprintln(out, "(the log holds no schema statements)")
	}
	return nil
}

// recoverAt opens a database as it was after the first n schema statements.
//
// It never writes and never compacts, so looking at the past cannot become the
// present. Copy the directory first anyway: the point of being here is that
// something has already gone wrong once.
func recoverAt(dir string, n int, stdin io.Reader, stdout, stderr io.Writer,
	format format, command, file string) error {
	eng, err := engine.OpenAt(dir, n)
	if err != nil {
		return err
	}
	defer func() { _ = eng.Close() }()

	db := sql.OpenDB(lightsqldriver.NewEngineConnector(eng))
	defer func() { _ = db.Close() }()

	_, _ = fmt.Fprintf(stderr,
		"opened %s as it was after %d schema statements; read-only, nothing is written\n", dir, n)

	sh := &shell{db: db, out: stdout, err: stderr, format: format, dsn: dir}
	switch {
	case command != "":
		return sh.repl(strings.NewReader(command), false)
	case file != "":
		src, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		return sh.repl(strings.NewReader(string(src)), false)
	default:
		return sh.repl(stdin, isTerminal(stdin))
	}
}

const usage = `lightsql is a shell for a lightsql database.

Usage:
  lightsql [flags] [path]

The path is a directory holding a database, created if it does not exist. With
no path the database is in memory and goes away when the shell exits.

To recover from a mistaken schema change, copy the log a compaction replaced
into a directory of its own and open it before the statement that did the
damage. Copy again for each attempt: opening a rescue directory the ordinary
way compacts it on close.

  cp demo.db/wal.old /tmp/rescue.db/wal
  lightsql -history /tmp/rescue.db     # what happened, numbered
  lightsql -at 2 /tmp/rescue.db        # as it was after the first two

Flags:
`

// dataSource turns the positional argument into a data source name.
//
// A path always gets the file: prefix. Without it the name would select an
// in-memory instance, which works perfectly until the process exits and takes
// the data with it -- the one mistake this tool must not make silently.
func dataSource(args []string, fsyncOn bool) (string, error) {
	if len(args) > 1 {
		return "", errors.New("expected at most one path")
	}
	if len(args) == 0 {
		return "memory:lightsql-shell", nil
	}

	path, err := filepath.Abs(args[0])
	if err != nil {
		return "", err
	}
	// A path that names an existing file rather than a directory is a mistake
	// worth catching here: a database is a directory, and the alternative is a
	// confusing error from inside the log.
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return "", fmt.Errorf("%s is a file; a lightsql database is a directory", args[0])
	}
	dsn := "file:" + path
	if !fsyncOn {
		dsn += "?fsync=off"
	}
	return dsn, nil
}

// isTerminal reports whether the shell should prompt. A pipe or a redirected
// file is a script, and printing a prompt into one is noise. Anything that is
// not a file at all is a test driving the shell, which does not want prompts
// either.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
