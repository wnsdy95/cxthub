package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type oauthGrantResourceStore interface {
	outbound.OAuthGrants
	UpsertUser(context.Context, domain.User) error
}

func TestFSOAuthGrantResourceImmutable(t *testing.T) {
	directory := t.TempDir()
	testOAuthGrantResourceImmutable(t, NewFSStore(directory), NewFSStore(directory))
}

func testOAuthGrantResourceImmutable(t *testing.T, storage, peer oauthGrantResourceStore) {
	t.Helper()
	ctx := context.Background()
	user := domain.User{ID: domain.NewID("user_"), Username: "resource" + domain.NewID("")[:10], Email: domain.NewID("resource") + "@example.test", Name: "Resource"}
	if err := storage.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	mutate := func(operation func(context.Context) error) error {
		if transactions, ok := storage.(outbound.IdentityTransactions); ok {
			return transactions.WithinIdentity(ctx, operation)
		}
		return operation(ctx)
	}
	for _, resource := range []string{"", "https://cxthub.test/mcp", "https://mcp.cxthub.test/mcp"} {
		now := time.Now().UTC().Truncate(time.Microsecond)
		grant := domain.OAuthGrant{ID: domain.NewID("grant_"), UserID: user.ID, ClientID: "resource-client", Resource: resource, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if err := mutate(func(ctx context.Context) error { return storage.CreateOAuthGrant(ctx, grant) }); err != nil {
			t.Fatal(err)
		}
		persisted, err := peer.GetOAuthGrant(ctx, grant.ID)
		if err != nil || persisted.Resource != resource {
			t.Fatalf("resource persistence: %q %v", persisted.Resource, err)
		}
		for _, replacement := range []string{"", "https://other.test/mcp"} {
			if replacement == resource {
				continue
			}
			modified := persisted
			modified.Resource = replacement
			modified.ExpiresAt = modified.ExpiresAt.Add(time.Hour)
			if err := mutate(func(ctx context.Context) error { return storage.UpdateOAuthGrant(ctx, modified) }); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("resource changed from %q to %q: %v", resource, replacement, err)
			}
			unchanged, err := peer.GetOAuthGrant(ctx, grant.ID)
			if err != nil || unchanged.Resource != resource || !unchanged.ExpiresAt.Equal(grant.ExpiresAt) {
				t.Fatalf("rejected update changed grant: %+v %v", unchanged, err)
			}
		}
		persisted.ExpiresAt = persisted.ExpiresAt.Add(time.Hour)
		if err := mutate(func(ctx context.Context) error { return storage.UpdateOAuthGrant(ctx, persisted) }); err != nil {
			t.Fatal("resource-preserving rotation failed", err)
		}
		revoked := now.Add(time.Second)
		persisted.RevokedAt = &revoked
		if err := mutate(func(ctx context.Context) error { return storage.UpdateOAuthGrant(ctx, persisted) }); err != nil {
			t.Fatal("resource-preserving revocation failed", err)
		}
		persisted, err = peer.GetOAuthGrant(ctx, grant.ID)
		if err != nil || persisted.Resource != resource || persisted.RevokedAt == nil {
			t.Fatalf("revoked grant persistence: %+v %v", persisted, err)
		}
	}
}
