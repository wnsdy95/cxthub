//go:build !postgres

package store

import "fmt"

// Open opens the server store (default build: always uses FSStore).
//
// The Postgres adapter is gated with //go:build postgres. To build with the Postgres adapter, use `go build -tags postgres`. In this case, Open in open_postgres.go is used instead, and if a DSN is provided, it opens a PostgresStore.
func Open(dataDir, dsn string, requirePostgres bool) (Store, error) {
	if dsn != "" || requirePostgres {
		return nil, fmt.Errorf("PostgreSQL storage requested but this cxtd binary was built without the postgres tag")
	}
	return OpenFSStore(dataDir)
}
