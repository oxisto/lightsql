package driver

import (
	"net/url"
	"strings"
	"sync"

	"github.com/oxisto/lightsql/internal/engine"
	"github.com/oxisto/lightsql/internal/pgerr"
)

// instances holds the named in-memory engines reachable through sql.Open.
//
// A registry is needed because sql.Open takes a string, and every connection
// derived from the same data source name must reach the same data. It is kept
// deliberately small and, crucially, droppable: an engine that can never be
// released leaks for the lifetime of the process, and a test suite that opens
// one instance per test would accumulate all of them.
var instances = struct {
	sync.Mutex
	byName map[string]*engine.Engine
}{byName: make(map[string]*engine.Engine)}

// instanceFor resolves a data source name to an engine, creating it on first
// use.
//
// The accepted forms are:
//
//	""                a shared instance named "default"
//	mytest            a named in-memory instance
//	memory:mytest     the same, written explicitly
//	file:./demo.db    a directory-backed instance (not yet implemented)
//
// The MySQL-derived grammar some Go drivers inherit, where a name encodes a
// protocol, socket path and credentials, is not accepted: none of it means
// anything to an in-process engine.
func instanceFor(dsn string) (*engine.Engine, error) {
	name, opts, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	if opts.file != "" {
		return nil, pgerr.New(pgerr.FeatureNotSupported,
			"file-backed instances are not implemented yet")
	}

	instances.Lock()
	defer instances.Unlock()

	if e, ok := instances.byName[name]; ok {
		return e, nil
	}
	e := engine.New()
	instances.byName[name] = e
	return e, nil
}

type dsnOptions struct {
	// file is the directory backing the instance, empty for in-memory.
	file string
}

func parseDSN(dsn string) (name string, opts dsnOptions, err error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "default", opts, nil
	}

	target := dsn
	if scheme, rest, ok := strings.Cut(dsn, ":"); ok {
		switch scheme {
		case "memory":
			target = rest
		case "file":
			opts.file = rest
			target = rest
		default:
			// Anything else is a plain name that happens to contain a colon.
		}
	}

	// Options are written as a query string, so a name may not contain one.
	base, query, hasQuery := strings.Cut(target, "?")
	if hasQuery {
		if _, err := url.ParseQuery(query); err != nil {
			return "", opts, pgerr.Newf(pgerr.SyntaxError,
				"invalid options in data source name: %v", err)
		}
	}
	if opts.file != "" {
		opts.file = base
	}
	if base == "" {
		return "", opts, pgerr.New(pgerr.SyntaxError, "data source name has an empty instance name")
	}
	return base, opts, nil
}

// Drop releases a named in-memory instance and everything in it.
//
// Without this, an engine reached by name lives as long as the process, because
// closing a *sql.DB cannot tell the driver that nothing will use the name again.
// A suite that creates an instance per test needs a way to let them go.
//
// It reports whether an instance was present.
func Drop(name string) bool {
	if name == "" {
		name = "default"
	}
	instances.Lock()
	defer instances.Unlock()

	_, ok := instances.byName[name]
	delete(instances.byName, name)
	return ok
}
