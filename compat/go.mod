// Module compat holds the test suites that need dependencies.
//
// It is a separate module so that importing lightsql pulls in nothing: the root
// go.mod is guarded by TestNoRuntimeDependencies, and a PostgreSQL driver or an
// ORM listed there would be inherited by every downstream project. Keeping them
// here is what lets the parity suite use a real database driver while the thing
// it is testing stays dependency-free.
module github.com/oxisto/lightsql/compat

go 1.26.5

// The suites test the working tree, not a published version.
replace github.com/oxisto/lightsql => ../

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/oxisto/lightsql v0.0.0-00010101000000-000000000000
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)
