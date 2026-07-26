package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestMemorySinksRefuseSymlinkTargets(t *testing.T) {
	digest := domain.MemoryDigest{Summary: "untrusted remote memory"}
	for _, tt := range []struct {
		name     string
		filename string
		inject   func(context.Context, domain.MemoryDigest, string) (string, error)
	}{
		{name: "claude", filename: "CLAUDE.md", inject: NewClaudeMemorySink().Inject},
		{name: "codex", filename: "AGENTS.md", inject: NewCodexMemorySink().Inject},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			outside := filepath.Join(t.TempDir(), "outside.txt")
			if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(repo, tt.filename)); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			if _, err := tt.inject(context.Background(), digest, repo); err == nil {
				t.Fatal("symlink target was accepted")
			}
			got, err := os.ReadFile(outside)
			if err != nil || string(got) != "keep" {
				t.Fatalf("outside target changed: %q err=%v", got, err)
			}
		})
	}
}
