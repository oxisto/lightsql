package driver

import (
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/oxisto/lightsql/internal/engine"
	"github.com/oxisto/lightsql/internal/pgerr"
)

// instances holds the engines reachable through sql.Open.
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
//	""                 a shared instance named "default"
//	mytest             a named in-memory instance
//	memory:mytest      the same, written explicitly
//	file:./demo.db     a directory holding a database that survives a restart
//	file:./demo.db?fsync=off
//
// The MySQL-derived grammar some Go drivers inherit, where a name encodes a
// protocol, socket path and credentials, is not accepted: none of it means
// anything to an in-process engine.
func instanceFor(dsn string) (*engine.Engine, error) {
	src, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}

	// The lock is held across recovery, which for a large database is the
	// slowest thing this package does. Releasing it first would let two
	// connections opening the same directory each replay the log and each build
	// an engine, and one of them would then be writing to a database nobody was
	// reading. Opening a second, unrelated instance waiting is the cheaper
	// problem.
	instances.Lock()
	defer instances.Unlock()

	if e, ok := instances.byName[src.key]; ok {
		return e, nil
	}

	var e *engine.Engine
	if src.dir == "" {
		e = engine.New()
	} else if e, err = engine.Open(src.dir, src.fsync); err != nil {
		return nil, err
	}
	instances.byName[src.key] = e
	return e, nil
}

// source is a resolved data source name.
type source struct {
	// key identifies the instance in the registry. It carries the scheme, so a
	// directory called mytest and an in-memory instance of that name are two
	// databases rather than one.
	key string
	// dir is the directory backing the instance, empty for in-memory.
	dir string
	// fsync says whether a commit waits for the disk.
	fsync bool
}

func parseDSN(dsn string) (source, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return source{key: "memory:default"}, nil
	}

	target, onDisk := dsn, false
	if scheme, rest, ok := strings.Cut(dsn, ":"); ok {
		switch scheme {
		case "memory":
			target = rest
		case "file":
			target, onDisk = rest, true
		default:
			// Anything else is a plain name that happens to contain a colon.
		}
	}

	// Options are written as a query string, so a name may not contain one.
	base, query, hasQuery := strings.Cut(target, "?")
	// A commit that has not reached the disk is not a commit, so syncing is on
	// unless it is turned off. Trading durability for speed has to be asked for
	// rather than arrived at by leaving something out.
	src := source{dir: base, fsync: true}
	if hasQuery {
		opts, err := url.ParseQuery(query)
		if err != nil {
			return source{}, pgerr.Newf(pgerr.SyntaxError,
				"invalid options in data source name: %v", err)
		}
		if err := src.applyOptions(opts); err != nil {
			return source{}, err
		}
	}
	if base == "" {
		return source{}, pgerr.New(pgerr.SyntaxError, "data source name has an empty instance name")
	}
	if !onDisk {
		return source{key: "memory:" + base}, nil
	}

	// The registry is keyed on the resolved path, so two data source names
	// reaching the same directory reach the same database rather than opening
	// it twice and having the second overwrite the first.
	abs, err := filepath.Abs(base)
	if err != nil {
		return source{}, pgerr.Newf(pgerr.SyntaxError, "resolving %q: %v", base, err)
	}
	src.dir, src.key = abs, "file:"+abs
	return src, nil
}

// applyOptions reads the query string of a data source name.
func (s *source) applyOptions(opts url.Values) error {
	for name, vals := range opts {
		switch name {
		case "fsync":
			on, err := parseBool(vals[len(vals)-1])
			if err != nil {
				return pgerr.Newf(pgerr.SyntaxError, "fsync: %v", err)
			}
			s.fsync = on
		default:
			// An unknown option is refused rather than ignored. A driver that
			// accepts and discards its own settings is how a database ends up
			// running without the durability its caller asked for.
			return pgerr.Newf(pgerr.SyntaxError, "unknown option %q in data source name", name)
		}
	}
	return nil
}

// parseBool accepts the spellings a data source name is likely to use, which is
// wider than strconv.ParseBool.
func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "on", "yes":
		return true, nil
	case "off", "no":
		return false, nil
	default:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return false, pgerr.Newf(pgerr.SyntaxError, "%q is not a boolean", s)
		}
		return b, nil
	}
}

// Drop releases an instance and everything in it.
//
// Without this, an engine reached by name lives as long as the process, because
// closing a *sql.DB cannot tell the driver that nothing will use the name again.
// A suite that creates an instance per test needs a way to let them go.
//
// A file-backed instance is checkpointed and closed, so the directory is left in
// a state that opens quickly. It takes the same data source name that opened it.
//
// It reports whether an instance was present.
func Drop(dsn string) bool {
	src, err := parseDSN(dsn)
	if err != nil {
		return false
	}

	instances.Lock()
	e, ok := instances.byName[src.key]
	delete(instances.byName, src.key)
	instances.Unlock()

	if !ok {
		return false
	}
	// The error is deliberately not returned. Drop is called from a deferred
	// cleanup where there is nothing useful to do with one, and a signature
	// that made every caller handle it would be answered with an empty check.
	_ = e.Close()
	return true
}
