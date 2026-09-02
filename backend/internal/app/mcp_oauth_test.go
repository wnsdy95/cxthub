package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMCPTokensAreHashedClientBoundAndRefreshIsNotBearer(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	user := domain.User{ID: "mcp-user", Email: "mcp@example.test", Name: "MCP User", Username: "mcp-user"}
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	svc := NewIdentityService(nil, st)
	pair, err := svc.IssueMCPTokenPair(ctx, user.ID, "mcp-client-a")
	if err != nil {
		t.Fatal(err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" || pair.ExpiresIn != 3600 {
		t.Fatalf("token pair = %+v", pair)
	}
	if _, err := st.GetSession(ctx, pair.AccessToken); !errors.Is(err, domain.ErrValidation) && !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("raw access token persisted: %v", err)
	}
	if _, err := svc.ResolveUser(ctx, pair.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("MCP access token crossed into REST authentication: %v", err)
	}
	if got, err := svc.ResolveMCPUser(ctx, pair.AccessToken); err != nil || got.ID != user.ID {
		t.Fatalf("MCP access resolve = %+v, %v", got, err)
	}
	if _, err := svc.ResolveSession(ctx, pair.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("MCP access token crossed into generic session auth: %v", err)
	}
	if _, err := svc.ResolveUser(ctx, pair.RefreshToken); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("refresh token accepted as bearer: %v", err)
	}
	if _, err := svc.RefreshMCPAccessToken(ctx, pair.RefreshToken, "mcp-client-b"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("refresh accepted by another client: %v", err)
	}
	refreshed, err := svc.RefreshMCPAccessToken(ctx, pair.RefreshToken, "mcp-client-a")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken == pair.AccessToken || refreshed.RefreshToken == pair.RefreshToken {
		t.Fatalf("refresh result = %+v", refreshed)
	}
	if _, err := svc.RefreshMCPAccessToken(ctx, pair.RefreshToken, "mcp-client-a"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("rotated refresh token was reusable: %v", err)
	}
	if err := svc.RevokeMCPToken(ctx, refreshed.AccessToken, "mcp-client-b"); err != nil {
		t.Fatalf("foreign-client revocation should be an idempotent no-op: %v", err)
	}
	if _, err := svc.ResolveMCPUser(ctx, refreshed.AccessToken); err != nil {
		t.Fatalf("foreign client revoked access token: %v", err)
	}
	if err := svc.RevokeMCPToken(ctx, refreshed.AccessToken, "mcp-client-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveMCPUser(ctx, refreshed.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("revoked access token still resolves: %v", err)
	}
	if err := svc.RevokeMCPToken(ctx, refreshed.RefreshToken, "mcp-client-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RefreshMCPAccessToken(ctx, refreshed.RefreshToken, "mcp-client-a"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("revoked refresh token still rotates: %v", err)
	}
}
