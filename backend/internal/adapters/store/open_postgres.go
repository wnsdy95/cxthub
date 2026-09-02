//go:build postgres

package store

import (
	"context"
	"fmt"
)

// Open(postgres build): if dsn exists, use PostgresStore, otherwise use FSStore.
// `go build -tags postgres` + CXT_POSTGRES_DSN to activate.
func Open(dataDir, dsn string, requirePostgres bool) (Store, error) {
	if dsn == "" {
		if requirePostgres {
			return nil, fmt.Errorf("CXT_POSTGRES_DSN is required when PostgreSQL storage is enforced")
		}
		return OpenFSStore(dataDir)
	}
	return NewPostgresStore(context.Background(), dsn)
}
