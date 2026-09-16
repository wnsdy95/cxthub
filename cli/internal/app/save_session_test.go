package app

import (
	"context"
	"errors"
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

type replaceBeforePendingCASStore struct {
	outbound.SessionStore
	replacement domain.Pending
	expected    domain.ContentHash
	calls       int
}

func (s *replaceBeforePendingCASStore) CompareAndDeletePending(ctx context.Context, repoID, sessionID string, expected domain.ContentHash) (bool, error) {
	s.calls++
	s.expected = expected
	if err := s.SessionStore.PutPending(ctx, s.replacement); err != nil {
		return false, err
	}
	return s.SessionStore.CompareAndDeletePending(ctx, repoID, sessionID, expected)
}

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
	saved, err := store.GetSnapshot(ctx, out.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.GetRef(ctx, saved.RepoID, domain.RefBranch, "main")
	if err != nil || ref.Target != out.SnapshotID {
		t.Fatalf("ref target mismatch: %+v / %v", ref, err)
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

func TestSaveCommitPreservesNewerConcurrentPendingCapture(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := t.TempDir()
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q", "-b", "main").Run(); err != nil {
		t.Skipf("git usage not possible: %v", err)
	}

	sessDir := filepath.Join(home, ".claude", "projects", encodeCwdLocal(cwd))
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "sess.jsonl"), []byte(e2eClaudeSession), 0o644); err != nil {
		t.Fatal(err)
	}

	base := storage.NewFileStore(repoRoot)
	store := &replaceBeforePendingCASStore{SessionStore: base}
	captures := map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture()}
	codecs := map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec()}
	svc := NewSaveSessionService(gitctx.NewGitContextAdapter(), captures, codecs, store)
	ctx := context.Background()

	pending, err := svc.Save(ctx, inbound.SaveInput{Cwd: cwd, Provider: domain.ProviderClaude, Message: domain.HookMessagePrefix + " test", Pending: true})
	if err != nil {
		t.Fatalf("pending save: %v", err)
	}
	newerTarget := domain.HashContent([]byte("capture that arrived during commit save"))
	store.replacement = domain.Pending{
		RepoID:    string(domain.HashContent([]byte(repoRoot))),
		SessionID: pending.SessionID,
		Branch:    pending.Branch,
		Provider:  domain.ProviderClaude,
		Target:    newerTarget,
	}

	committed, err := svc.Save(ctx, inbound.SaveInput{Cwd: cwd, Provider: domain.ProviderClaude, Message: "commit"})
	if err != nil {
		t.Fatalf("commit save: %v", err)
	}
	if store.calls != 1 || store.expected != pending.SnapshotID {
		t.Fatalf("pending CAS calls=%d expected=%s, want one call for %s", store.calls, store.expected, pending.SnapshotID)
	}
	if committed.ResolvedPendingTarget != pending.SnapshotID {
		t.Fatalf("resolved target=%s, want observed pending %s", committed.ResolvedPendingTarget, pending.SnapshotID)
	}
	pendings, err := base.ListPendings(ctx, "")
	if err != nil || len(pendings) != 1 || pendings[0].Target != newerTarget {
		t.Fatalf("newer pending capture was not preserved: %+v err=%v", pendings, err)
	}
}

func TestSaveRejectsCrossProviderNativeSessionCollision(t *testing.T) {
	for _, pending := range []bool{true, false} {
		t.Run(map[bool]string{true: "capture", false: "commit"}[pending], func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			t.Setenv("HOME", home)
			cwd := t.TempDir()
			if err := exec.Command("git", "-C", cwd, "init", "-q", "-b", "main").Run(); err != nil {
				t.Fatal(err)
			}
			claudePath, codexPath := filepath.Join(home, "claude.jsonl"), filepath.Join(home, "codex.jsonl")
			codexRaw := `{"timestamp":"2026-06-30T01:00:00Z","type":"session_meta","payload":{"id":"s1","cwd":"/work/proj","model":"gpt-5-codex"}}
{"timestamp":"2026-06-30T01:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"independent Codex conversation"}]}}`
			for path, raw := range map[string]string{claudePath: e2eClaudeSession, codexPath: codexRaw} {
				if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			st := storage.NewFileStore(cwd)
			svc := NewSaveSessionService(gitctx.NewGitContextAdapter(),
				map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture(), domain.ProviderCodex: capture.NewCodexCapture()},
				map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec(), domain.ProviderCodex: codec.NewCodexCodec()}, st)
			first, err := svc.Save(ctx, inbound.SaveInput{Cwd: cwd, Provider: domain.ProviderClaude, SessionPath: claudePath, Pending: true, Message: domain.HookMessagePrefix + " capture"})
			if err != nil {
				t.Fatal(err)
			}
			if first.SessionID != "s1" {
				t.Fatalf("fixture native session ID=%q", first.SessionID)
			}
			_, err = svc.Save(ctx, inbound.SaveInput{Cwd: cwd, Provider: domain.ProviderCodex, SessionPath: codexPath, Pending: pending, Message: domain.HookMessagePrefix + " capture"})
			if !errors.Is(err, domain.ErrSyncConflict) {
				t.Errorf("cross-provider save was accepted: %v", err)
			}
			got, err := st.ListPendings(ctx, "")
			if err != nil || len(got) != 1 || got[0].Target != first.SnapshotID || got[0].Provider != domain.ProviderClaude {
				t.Errorf("original pointer lost: %+v err=%v", got, err)
			}
			if _, err := st.GetSnapshot(ctx, first.SnapshotID); err != nil {
				t.Errorf("original snapshot lost: %v", err)
			}
			if _, err := st.GetDoc(ctx, first.SnapshotID); err != nil {
				t.Errorf("original document lost: %v", err)
			}
			refs, err := st.ListRefs(ctx, "")
			if err != nil || len(refs) != 0 {
				t.Errorf("collision published refs: %+v err=%v", refs, err)
			}
		})
	}
}
