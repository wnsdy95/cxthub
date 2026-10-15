//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGOAuthGrantResourceUpgradeAndImmutability(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("requires disposable CXT_TEST_DSN")
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	database := pgx.Identifier{domain.NewID("oauth_resource_upgrade_")}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+database); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	defer func() {
		defer admin.Close()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+database); err != nil {
			t.Error(err)
		}
	}()
	config.ConnConfig.Database = strings.Trim(database, `"`)
	pool, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	storage := &PostgresStore{pool: pool}
	defer storage.Close()
	peerPool, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	peer := &PostgresStore{pool: peerPool}
	defer peer.Close()
	migrations := "../../../../schemas/db/migrations"
	legacyMigrations := t.TempDir()
	entries, err := os.ReadDir(migrations)
	if err != nil {
		t.Fatal(err)
	}
	pendingMigrations := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if entry.Name() >= "0092_oauth_grant_resource.sql" {
			pendingMigrations++
			continue
		}
		contents, err := os.ReadFile(filepath.Join(migrations, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacyMigrations, entry.Name()), contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if applied, err := storage.ApplyMigrations(ctx, legacyMigrations); err != nil || applied != 91 {
		t.Fatalf("pre-cutover migration count=%d err=%v", applied, err)
	}
	user := domain.User{ID: domain.NewID("user_"), Username: "upgrade" + domain.NewID("")[:10], Name: "Upgrade", Email: "upgrade@example.test"}
	if err := storage.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	legacyID := domain.NewID("grant_")
	if _, err := pool.Exec(ctx, `INSERT INTO oauth_grants(id,user_id,client_id,created_at,expires_at) VALUES($1,$2,$3,$4,$5)`, legacyID, user.ID, "upgrade-client", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if applied, err := storage.ApplyMigrations(ctx, migrations); err != nil || applied != pendingMigrations || pendingMigrations == 0 {
		t.Fatalf("additive migration count=%d err=%v", applied, err)
	}
	if applied, err := storage.ApplyMigrations(ctx, migrations); err != nil || applied != 0 {
		t.Fatalf("migration idempotency count=%d err=%v", applied, err)
	}
	legacy, err := peer.GetOAuthGrant(ctx, legacyID)
	if err != nil || legacy.Resource != "" || legacy.UserID != user.ID || !legacy.CreatedAt.Equal(now) || legacy.RevokedAt != nil {
		t.Fatalf("legacy grant changed during migration: %+v %v", legacy, err)
	}
	testOAuthGrantResourceImmutable(t, storage, peer)
	bound := domain.OAuthGrant{ID: domain.NewID("grant_"), UserID: user.ID, ClientID: "upgrade-client", Resource: "https://mcp.cxthub.test/mcp", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := storage.WithinIdentity(ctx, func(ctx context.Context) error { return storage.CreateOAuthGrant(ctx, bound) }); err != nil {
		t.Fatal(err)
	}
	for _, grant := range []domain.OAuthGrant{legacy, bound} {
		for _, replacement := range []any{"https://other.test/mcp", "", nil} {
			if replacement == grant.Resource {
				continue
			}
			_, err := pool.Exec(ctx, `UPDATE oauth_grants SET resource=$2 WHERE id=$1`, grant.ID, replacement)
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || failure.Code != "23514" {
				t.Fatalf("SQL resource mutation accepted: %v", err)
			}
		}
		persisted, err := peer.GetOAuthGrant(ctx, grant.ID)
		if err != nil || persisted.Resource != grant.Resource {
			t.Fatalf("SQL mutation changed resource: %q %v", persisted.Resource, err)
		}
	}
}
