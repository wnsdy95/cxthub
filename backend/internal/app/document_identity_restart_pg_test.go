//go:build postgres

package app

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Each case exercises the real global claim against only its own durable queue.
// The configured database may contain other suites' pending jobs; never process
// or delete them. Match the existing catalog-upgrade disposable database pattern.
func a11IsolatedPGDSN(t *testing.T, ctx context.Context) string {
	t.Helper()
	dsn := collaborationDSN(t)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	database := domain.NewID("root_startup_")
	quoted := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.Exec(cleanup, "DROP DATABASE "+quoted)
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + database
		q := u.Query()
		q.Set("dbname", database)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " dbname=" + database
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != database {
		t.Fatal("isolated DSN did not select the owned database", err)
	}
	t.Logf("isolated global queue in owned database %s", database)
	return dsn
}
