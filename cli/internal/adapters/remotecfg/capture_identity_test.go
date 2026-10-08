package remotecfg

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureIdentityConfigBindingAndCAS(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const origin = "https://capture.example.test/team/repo"
	if err := configFixtureSave(root, Remotes{"origin": origin}); err != nil {
		t.Fatal(err)
	}
	if err := SetSecretsRedact(ctx, root, "preserved"); err != nil {
		t.Fatal(err)
	}
	selected := domain.Repo{ID: RepoIDFor(origin), RemoteURL: origin}
	if got, err := captureIdentity(ctx, root, selected); err != nil || got != "" {
		t.Fatal(got, err)
	}
	observed, err := Observe(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetCaptureDocumentIdentity(ctx, observed, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if got, err := captureIdentity(ctx, root, selected); err != nil || got != domain.DocumentIdentityRootV1 {
		t.Fatal(got, err)
	}
	if SecretsRedact(root) != "preserved" {
		t.Fatal("lost other config")
	}
	if err := SetCaptureDocumentIdentity(ctx, observed, domain.DocumentIdentityLegacy); !errors.Is(err, ErrChanged) {
		t.Fatal("stale confirmation published", err)
	}
	other := selected
	other.ID = string(domain.HashContent([]byte("other")))
	if got, err := captureIdentity(ctx, root, other); err != nil || got != "" {
		t.Fatal("wrong repo inherited root", got, err)
	}
	if err := configFixtureSave(root, Remotes{"origin": "https://other.example.test/team/repo"}); err != nil {
		t.Fatal(err)
	}
	if got, err := captureIdentity(ctx, root, selected); err != nil || got != "" {
		t.Fatal("endpoint switch inherited root", got, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := captureIdentity(canceled, root, selected); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	observed, err = Observe(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetCaptureDocumentIdentity(ctx, observed, domain.DocumentIdentityLegacy); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".cxt", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || SecretsRedact(root) != "preserved" {
		t.Fatal("clear damaged config")
	}
}
func TestCaptureIdentityMalformedCannotSelectRoot(t *testing.T) {
	for _, value := range []any{map[string]any{"version": 1, "identity": "unknown"}, map[string]any{"version": 2, "identity": string(domain.DocumentIdentityRootV1)}, map[string]any{"version": 1, "identity": string(domain.DocumentIdentityRootV1), "repo_id": "invalid"}, "root"} {
		root := t.TempDir()
		ctx := context.Background()
		if err := setField(ctx, root, "capture_document", value); err != nil {
			t.Fatal(err)
		}
		if _, err := CaptureIdentity(ctx, root); err == nil {
			t.Fatalf("accepted malformed preference %#v", value)
		}
	}
}
