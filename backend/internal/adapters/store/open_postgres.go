//go:build postgres

package store

import "context"

// Open(postgres build): if dsn exists, use PostgresStore, otherwise use FSStore.
// `go build -tags postgres` + CXT_POSTGRES_DSN to activate.
func Open(dataDir, dsn string) (Store, error) {
	if dsn == "" {
		return OpenFSStore(dataDir)
	}
	return NewPostgresStore(context.Background(), dsn)
}
