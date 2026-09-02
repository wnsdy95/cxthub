//go:build !postgres

package store

import "fmt"

// Open opens the loopback-development store (default build: FSStore only).
//
// The Postgres adapter is gated with //go:build postgres. External cxtd binds
// always request PostgreSQL, so this build fails closed instead of serving FS.
func Open(dataDir, dsn string, requirePostgres bool) (Store, error) {
	if dsn != "" || requirePostgres {
		return nil, fmt.Errorf("PostgreSQL storage requested but this cxtd binary was built without the postgres tag")
	}
	return OpenFSStore(dataDir)
}
