package capture

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func settingsBundle(kind string, files map[string]string) domain.SettingsBundle {
	bundle := domain.SettingsBundle{Kind: kind}
	for path, content := range files {
		bundle.Files = append(bundle.Files, domain.SettingsFile{
			Path: path, ContentB64: base64.StdEncoding.EncodeToString([]byte(content)),
		})
	}
	return bundle
}

func TestWriteSettingsDirRejectsMaliciousKindBeforeMutation(t *testing.T) {
	repo := t.TempDir()
	existing := filepath.Join(repo, ".claude", "keep.md")
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(repo), "target")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(outside, "marker")
	if err := os.WriteFile(marker, []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}

	malicious := settingsBundle("../../../target", map[string]string{"owned": "bad"})
	if _, err := WriteSettingsDir(repo, "claude", "", malicious); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("malicious kind error = %v", err)
	}
	if got, err := os.ReadFile(existing); err != nil || string(got) != "keep" {
		t.Fatalf("existing settings changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "safe" {
		t.Fatalf("outside directory changed: %q, %v", got, err)
	}
}

func TestWriteSettingsDirValidationFailureKeepsExistingDirectory(t *testing.T) {
	repo := t.TempDir()
	existing := filepath.Join(repo, ".claude", "keep.md")
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := domain.SettingsBundle{Kind: "claude", Files: []domain.SettingsFile{{Path: "../outside", ContentB64: ""}}}
	if _, err := WriteSettingsDir(repo, "claude", "", bad); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("unsafe path error = %v", err)
	}
	if got, err := os.ReadFile(existing); err != nil || string(got) != "keep" {
		t.Fatalf("existing settings changed: %q, %v", got, err)
	}
}

func TestWriteSettingsDirReplacesOnlyExpectedKind(t *testing.T) {
	repo := t.TempDir()
	stale := filepath.Join(repo, ".claude", "stale.md")
	agents := filepath.Join(repo, ".agents", "keep.md")
	for _, path := range []string{stale, agents} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bundle := settingsBundle("claude", map[string]string{"commands/review.md": "new"})
	hash, _ := domain.SettingsObjectHash(bundle)
	n, err := WriteSettingsDir(repo, "claude", hash, bundle)
	if err != nil || n != 1 {
		t.Fatalf("write = %d, %v", n, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file survived replacement: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, ".claude", "commands", "review.md")); err != nil || string(got) != "new" {
		t.Fatalf("new file = %q, %v", got, err)
	}
	if got, err := os.ReadFile(agents); err != nil || string(got) != "old" {
		t.Fatalf("other settings kind changed: %q, %v", got, err)
	}
}
