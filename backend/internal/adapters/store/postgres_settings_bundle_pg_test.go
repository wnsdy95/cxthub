//go:build postgres

package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A repository's default agent settings are read as they were stored, by
// kind, and a stored row that is not a valid bundle is refused, not returned.
func TestPGSettingsBundleIsReadAsItWasStored(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	other, _ := rootPGRepo(t, ctx, storage)

	if _, err := storage.GetSettingsBundle(ctx, repo, "claude"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("before anything was stored: %v", err)
	}
	claude := domain.SettingsBundle{Kind: "claude", Files: []domain.SettingsFile{
		{Path: "settings.json", ContentB64: "eyJtb2RlbCI6InRlc3QifQ=="},
		{Path: "commands/review.md", ContentB64: "UmV2aWV3Lg=="},
	}}
	if err := storage.PutSettingsBundle(ctx, repo, claude); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetSettingsBundle(ctx, repo, "claude"); err != nil || !reflect.DeepEqual(got, claude) {
		t.Fatalf("read back: %+v %v", got, err)
	}
	// Each kind is its own bundle, and each repository its own.
	for _, kind := range []string{"agents", "codex"} {
		if _, err := storage.GetSettingsBundle(ctx, repo, kind); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("kind %s was answered with another kind's bundle: %v", kind, err)
		}
	}
	if _, err := storage.GetSettingsBundle(ctx, other, "claude"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another repository read this one's settings: %v", err)
	}
	codex := domain.SettingsBundle{Kind: "codex", Files: []domain.SettingsFile{{Path: "config.toml", ContentB64: "bW9kZWwgPSAidGVzdCI="}}}
	if err := storage.PutSettingsBundle(ctx, repo, codex); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetSettingsBundle(ctx, repo, "codex"); err != nil || !reflect.DeepEqual(got, codex) {
		t.Fatalf("the second kind: %+v %v", got, err)
	}
	if got, err := storage.GetSettingsBundle(ctx, repo, "claude"); err != nil || !reflect.DeepEqual(got, claude) {
		t.Fatalf("the first kind after the second was stored: %+v %v", got, err)
	}
	// Storing a kind again replaces it.
	replaced := domain.SettingsBundle{Kind: "claude", Files: []domain.SettingsFile{{Path: "settings.json", ContentB64: "e30="}}}
	if err := storage.PutSettingsBundle(ctx, repo, replaced); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repo_settings WHERE repo_id=$1`, string(repo)).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("rows after a replacement: %d %v", rows, err)
	}
	if got, err := storage.GetSettingsBundle(ctx, repo, "claude"); err != nil || !reflect.DeepEqual(got, replaced) {
		t.Fatalf("the replaced bundle: %+v %v", got, err)
	}

	// What cannot be asked.
	if _, err := storage.GetSettingsBundle(ctx, repo, "cursor"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a kind that does not exist: %v", err)
	}
	if _, err := storage.GetSettingsBundle(ctx, "not-a-hash", "claude"); err == nil {
		t.Fatal("a repository that is not a hash was read")
	}

	// A row that is not a valid bundle is refused when it is read.
	for name, data := range map[string]string{
		"a bundle of another kind":      `{"kind":"codex","files":[]}`,
		"a path that leaves the folder": `{"kind":"claude","files":[{"path":"../outside","content_b64":"e30="}]}`,
		"content that is not base64":    `{"kind":"claude","files":[{"path":"settings.json","content_b64":"%%%"}]}`,
		"the same path twice":           `{"kind":"claude","files":[{"path":"a.json","content_b64":"e30="},{"path":"a.json","content_b64":"e30="}]}`,
	} {
		if _, err := storage.pool.Exec(ctx, `UPDATE repo_settings SET data=$3 WHERE repo_id=$1 AND kind=$2`, string(repo), "claude", data); err != nil {
			t.Fatal(name, err)
		}
		if got, err := storage.GetSettingsBundle(ctx, repo, "claude"); !errors.Is(err, domain.ErrIntegrity) || len(got.Files) != 0 || got.Kind != "" {
			t.Errorf("%s was returned: %+v %v", name, got, err)
		}
	}
	// The store refuses to write such a bundle in the first place.
	if err := storage.PutSettingsBundle(ctx, repo, domain.SettingsBundle{Kind: "claude", Files: []domain.SettingsFile{{Path: "../outside", ContentB64: "e30="}}}); err == nil {
		t.Fatal("a bundle with a path that leaves the folder was stored")
	}
	if err := storage.PutSettingsBundle(ctx, repo, domain.SettingsBundle{Kind: "cursor"}); err == nil || strings.Contains(err.Error(), "SQLSTATE") {
		t.Fatalf("a bundle of a kind that does not exist: %v", err)
	}
}
