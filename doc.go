// Package lightsql is a small, embeddable SQL engine that speaks the PostgreSQL
// dialect and plugs into database/sql.
//
// It is aimed at two situations: unit tests that need a real SQL engine without
// a container, and small demo deployments whose data fits in memory. It is not a
// general-purpose database; see the non-goals in README.md.
//
// The engine can be used two ways. The preferred form builds a connector
// explicitly, which keeps configuration in Go and involves no global state:
//
//	db := sql.OpenDB(lightsql.NewConnector(lightsql.WithMemory()))
//
// The convenience form registers a driver under the name "lightsql", where the
// data source name selects a named in-memory instance or a directory:
//
//	db, err := sql.Open("lightsql", "mytest")
//	db, err := sql.Open("lightsql", "file:./demo.db")
//
// A directory-backed database appends every committed transaction to a
// write-ahead log before reporting the commit, and rebuilds itself from that log
// when it is opened again.
//
// The public API is still being built out; today this package only carries the
// module's documentation and the invariants enforced by its tests.
package lightsql
