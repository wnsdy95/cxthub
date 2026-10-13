//go:build postgres

package mcp

import (
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

func TestPGMCPResourceCutover(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("requires disposable CXT_TEST_DSN")
	}
	ctx := systemTestContext()
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, err = storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	runMCPResourceCutover(t, storage, peer)
}
