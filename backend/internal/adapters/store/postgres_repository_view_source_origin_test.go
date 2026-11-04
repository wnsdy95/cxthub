//go:build postgres

package store

import (
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGRepositoryViewSourceOriginsExcludeCredentials(t *testing.T) {
	store, ctx, repo := catalogPG(t)
	for _, raw := range []string{
		"https://GitHub.com/Acme/Orders.git",
		"https://oauth-user:super-secret@GitHub.com/Acme/Orders.git?token=also-secret#fragment",
		"https://github.com/Acme/Orders.git?",
		"ssh://git@github.com:22/Acme/Orders.git",
		"git+ssh://user:secret@github.com/Acme/Orders.git",
		"git://github.com/Acme/Orders.git",
		"git@github.com:Acme/Orders.git",
		"deploy-token@gitlab.example:Acme/Orders.git",
		"/Users/alice/private/repo", "file:///Users/alice/private/repo", "https://token@", "token@host:",
	} {
		t.Run(raw, func(t *testing.T) {
			var got string
			if err := store.pool.QueryRow(ctx, `SELECT cxt_repository_view_remote($1)`, raw).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if want := domain.SanitizeRemoteURL(raw); got != want {
				t.Fatalf("origin projection mismatch: got %q want %q", got, want)
			}
		})
	}
	for _, raw := range []string{
		"git@github.com:Acme/Orders.git?token=secret", "https://secret@bad host/repo", "javascript:secret", "https://%secret/repo",
	} {
		var got string
		if err := store.pool.QueryRow(ctx, `SELECT cxt_repository_view_remote($1)`, raw).Scan(&got); err != nil || got != "" {
			t.Fatalf("unsafe origin retained: %q %v", got, err)
		}
	}
	raw := "https://oauth-user:super-secret@github.com/acme/repo?token=also-secret#fragment"
	catalogExecPG(t, ctx, store.pool, `UPDATE repos SET remote_url=$2,git_remote_url=$2 WHERE id=$1`, string(repo), raw)
	var image, stored string
	if err := store.pool.QueryRow(ctx, `SELECT after_image::text FROM repository_view_source_changes WHERE repo_id=$1 AND entity_kind='repository' ORDER BY seq DESC LIMIT 1`, string(repo)).Scan(&image); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(image, "secret") || strings.Contains(image, "oauth-user") || !strings.Contains(image, "https://github.com/acme/repo") {
		t.Fatalf("credential copied into source journal: %s", image)
	}
	if err := store.pool.QueryRow(ctx, `SELECT git_remote_url FROM repos WHERE id=$1`, string(repo)).Scan(&stored); err != nil || stored != raw {
		t.Fatalf("projection rewrote source row: %q %v", stored, err)
	}
	catalogExecPG(t, ctx, store.pool, `UPDATE repos SET git_remote_url='ssh://[::1]/repo' WHERE id=$1`, string(repo))
	var supported bool
	if err := store.pool.QueryRow(ctx, `SELECT (after_image->>'git_remote_url_supported')::boolean FROM repository_view_source_changes WHERE repo_id=$1 AND entity_kind='repository' ORDER BY seq DESC LIMIT 1`, string(repo)).Scan(&supported); err != nil || supported {
		t.Fatalf("unsupported origin silently treated as absent: %v %v", supported, err)
	}
}
