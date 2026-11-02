//go:build postgres

package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

func TestPGLegacySessionLogoutRace(test *testing.T) {
	ctx := context.Background()
	storage, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(peer.Close)
	checkLegacySessionLogoutRace(test, storage, peer)
	checkLegacySessionLogoutSingleRevocation(test, storage)
	checkLegacySessionRevocationOrders(test, storage)
	checkLegacySessionMigrationIdentity(test, storage, true)
}
