package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// encodeCwdLocal is a test copy of capture.encodeCwd (non-alphanumeric characters are replaced with '-').
func encodeCwdLocal(p string) string {
	out := make([]rune, 0, len(p))
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}

const e2eClaudeSession = `{"type":"user","cwd":"/Users/work/proj","sessionId":"s1","gitBranch":"main","timestamp":"2026-06-30T00:00:00Z","message":{"role":"user","content":"hello"}}
{"type":"assistant","cwd":"/Users/work/proj","sessionId":"s1","gitBranch":"main","timestamp":"2026-06-30T00:00:01Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"hi there"}]}}`

// TestSaveEndToEnd: Capture → Decode → Save snapshot to .cxt to verify actual end-to-end operation.
func TestSaveEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := t.TempDir()
	// .git truth policy: cxt operates within a git repository, so cwd is set to the actual git repo.
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q", "-b", "main").Run(); err != nil {
		t.Skipf("git usage not possible: %v", err)
	}

	// 1) Fake active session batch: ~/.claude/projects/<encoded>/sess.jsonl
	sessDir := filepath.Join(home, ".claude", "projects", encodeCwdLocal(cwd))
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "sess.jsonl"), []byte(e2eClaudeSession), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2) Service assembly (actual adapter)
	store := storage.NewFileStore(repoRoot)
	captures := map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture()}
	codecs := map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec()}
	svc := NewSaveSessionService(gitctx.NewGitContextAdapter(), captures, codecs, store)

	// 3) Execute save
	ctx := context.Background()
	out, err := svc.Save(ctx, inbound.SaveInput{Cwd: cwd, Provider: domain.ProviderClaude, Message: "e2e"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if out.Branch != "main" {
		t.Fatalf("branch from gitBranch field expected 'main', got %q", out.Branch)
	}
	if out.SnapshotID == "" {
		t.Fatal("empty snapshot id")
	}

	// 4) .cxt disk verification
	hexID := strings.TrimPrefix(string(out.SnapshotID), "sha256:")
	if _, err := os.Stat(filepath.Join(repoRoot, ".cxt", "objects", "docs", hexID)); err != nil {
		t.Fatalf("doc object not stored: %v", err)
	}
	refData, err := os.ReadFile(filepath.Join(repoRoot, ".cxt", "refs", "heads", "main"))
	if err != nil {
		t.Fatalf("branch ref not written: %v", err)
	}
	if strings.TrimSpace(string(refData)) != string(out.SnapshotID) {
		t.Fatalf("ref target mismatch: %q vs %q", strings.TrimSpace(string(refData)), out.SnapshotID)
	}

	// 5) list by query
	list := NewListSessionsService(store)
	lo, err := list.List(ctx, inbound.ListInput{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(lo.Snapshots) != 1 || lo.Snapshots[0].Message != "e2e" {
		t.Fatalf("expected 1 snapshot 'e2e', got %+v", lo.Snapshots)
	}

	// 6) re-save same session → dedup (duplicate snapshot ID)
	out2, err := svc.Save(ctx, inbound.SaveInput{Cwd: cwd, Provider: domain.ProviderClaude, Message: "again"})
	if err != nil {
		t.Fatalf("Save2: %v", err)
	}
	if out2.SnapshotID != out.SnapshotID {
		t.Fatalf("identical session must dedup to same snapshot id")
	}
}
