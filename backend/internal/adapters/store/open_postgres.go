//go:build postgres

package store

import (
	"context"
	"fmt"
)

// Open(postgres build) uses PostgreSQL when a DSN is present. A missing DSN is
// allowed only for a loopback development caller that did not require
// PostgreSQL; cxtd marks every external bind as requirePostgres.
func Open(dataDir, dsn string, requirePostgres bool) (Store, error) {
	if dsn == "" {
		if requirePostgres {
			return nil, fmt.Errorf("CXT_POSTGRES_DSN is required when PostgreSQL storage is enforced")
		}
		return OpenFSStore(dataDir)
	}
	return NewPostgresStore(context.Background(), dsn)
}
