package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestStashUsesExplicitWrapperOwnedSessionPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q", "-b", "main").Run(); err != nil {
		t.Skipf("git usage not possible: %v", err)
	}
	dir := filepath.Join(home, ".claude", "projects", encodeCwdLocal(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ownedID := "11111111-1111-4111-8111-111111111111"
	siblingID := "22222222-2222-4222-8222-222222222222"
	owned := writeClaudeSession(t, dir, cwd, ownedID, "owned", time.Unix(1000, 0))
	writeClaudeSession(t, dir, cwd, siblingID, "newer sibling", time.Unix(2000, 0))

	store := storage.NewFileStore(t.TempDir())
	service := NewStashService(
		gitctx.NewGitContextAdapter(),
		map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture()},
		map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec()},
		store,
		nil,
	)
	out, err := service.Stash(context.Background(), inbound.StashInput{
		Cwd:         cwd,
		Provider:    domain.ProviderClaude,
		SessionPath: owned,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.GetSnapshot(context.Background(), out.StashID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SessionID != ownedID {
		t.Fatalf("stashed session = %q, want wrapper-owned %q", snapshot.SessionID, ownedID)
	}
}

func writeClaudeSession(t *testing.T, dir, cwd, sessionID, text string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, sessionID+".jsonl")
	raw := fmt.Sprintf("{\"type\":\"user\",\"cwd\":%q,\"sessionId\":%q,\"gitBranch\":\"main\",\"message\":{\"role\":\"user\",\"content\":%q}}\n", cwd, sessionID, text)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}
