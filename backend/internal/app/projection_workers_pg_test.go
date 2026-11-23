//go:build postgres

package app

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func wave40IsolatedPG(test *testing.T) {
	test.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("CXT_TEST_DSN unset; Wave40 proofs require a local PostgreSQL DSN and a fresh owned database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		test.Fatal("invalid local CXT_TEST_DSN")
	}
	local := func(host string) bool {
		if host == "localhost" || strings.HasPrefix(host, "/") {
			return true
		}
		address := net.ParseIP(host)
		return address != nil && address.IsLoopback()
	}
	if !local(config.ConnConfig.Host) {
		test.Fatal("Wave40 proofs only accept a local CXT_TEST_DSN")
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		if !local(fallback.Host) {
			test.Fatal("Wave40 proofs refuse a nonlocal fallback")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	test.Setenv("CXT_TEST_DSN", a11IsolatedPGDSN(test, ctx))
}

func wave40PGContext(ctx context.Context, storage *store.PostgresStore) context.Context {
	supported := ReadDocumentIdentities(storage, storage)
	ctx = inbound.WithDocumentIdentities(inbound.WithSystemActor(ctx), supported)
	return outbound.WithDocumentIdentityCompatibility(ctx, supported, supported)
}
