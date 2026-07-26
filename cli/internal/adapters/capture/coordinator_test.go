package capture

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// fakeSave records SaveSession calls as a fake.
type fakeSave struct {
	calls []inbound.SaveInput
	err   error
}

func (f *fakeSave) Save(_ context.Context, in inbound.SaveInput) (inbound.SaveOutput, error) {
	f.calls = append(f.calls, in)
	return inbound.SaveOutput{SnapshotID: "sha256:fake", Branch: "main"}, f.err
}

func newTestCoord(t *testing.T) (*CaptureCoordinator, *fakeSave, string, string) {
	t.Helper()
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".cxt"), 0o755); err != nil { // cxt active repo (opt-in gate)
		t.Fatal(err)
	}
	fs := &fakeSave{}
	coord := NewCaptureCoordinator(fs, domain.TeamIdentity{})
	session := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(session, []byte("{\"type\":\"user\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return coord, fs, cwd, session
}

func TestRequestCaptureBasic(t *testing.T) {
	coord, fs, cwd, session := newTestCoord(t)
	ctx := context.Background()
	if _, err := coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, true, false); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("expected 1 save, got %d", len(fs.calls))
	}
	in := fs.calls[0]
	if in.SessionPath != session || !in.Pending {
		t.Errorf("save input wrong: %+v", in)
	}
	if in.Message != domain.HookMessagePrefix+"checkpoint" {
		t.Errorf("default message wrong: %q", in.Message)
	}
	for _, ext := range []string{".last", ".cursor"} {
		if _, err := os.Stat(filepath.Join(cwd, ".cxt", "capture", "claude"+ext)); err != nil {
			t.Errorf("state file %s missing: %v", ext, err)
		}
	}
}

func TestRequestCaptureDebounce(t *testing.T) {
	coord, fs, cwd, session := newTestCoord(t)
	ctx := context.Background()
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, true, false)
	// Skip if file shrank but not inside Windows.
	_ = os.WriteFile(session, []byte("{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n"), 0o644)
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, true, false)
	if len(fs.calls) != 1 {
		t.Fatalf("debounce failed: %d saves", len(fs.calls))
	}
	// Recapture if outside Windows (.last mtime modified in the past).
	last := filepath.Join(cwd, ".cxt", "capture", "claude.last")
	old := time.Now().Add(-2 * time.Minute)
	_ = os.Chtimes(last, old, old)
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, true, false)
	if len(fs.calls) != 2 {
		t.Fatalf("post-window capture failed: %d saves", len(fs.calls))
	}
}

func TestRequestCaptureGrowthGate(t *testing.T) {
	coord, fs, cwd, session := newTestCoord(t)
	ctx := context.Background()
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, false, true)
	// No-op if file didn't shrink (content unchanged → dedup target, I/O savings).
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, false, true)
	if len(fs.calls) != 1 {
		t.Fatalf("growth gate failed: %d saves", len(fs.calls))
	}
	// If true, force ignores debouncing and captures immediately.
	_ = os.WriteFile(session, []byte("{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n"), 0o644)
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, false, true)
	if len(fs.calls) != 2 {
		t.Fatalf("force flush failed: %d saves", len(fs.calls))
	}
}

func TestTurnHintConsumed(t *testing.T) {
	coord, fs, cwd, session := newTestCoord(t)
	ctx := context.Background()
	if err := coord.MarkTurn(ctx, domain.ProviderCodex, cwd, "fix the parser bug\nsecond line ignored"); err != nil {
		t.Fatal(err)
	}
	_, _ = coord.RequestCapture(ctx, domain.ProviderCodex, cwd, session, true, false)
	if len(fs.calls) != 1 || fs.calls[0].Message != domain.HookMessagePrefix+"fix the parser bug" {
		t.Fatalf("turn hint not used: %+v", fs.calls)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cxt", "capture", "codex.turn")); !os.IsNotExist(err) {
		t.Error("turn hint not consumed (file remains)")
	}
}

func TestMarkBaselineClearsTurn(t *testing.T) {
	coord, _, cwd, session := newTestCoord(t)
	ctx := context.Background()
	_ = coord.MarkTurn(ctx, domain.ProviderClaude, cwd, "stale hint")
	if err := coord.MarkBaseline(ctx, domain.ProviderClaude, cwd, session); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cxt", "capture", "claude.turn")); !os.IsNotExist(err) {
		t.Error("stale turn hint not cleared at session boundary")
	}
	b, err := os.ReadFile(filepath.Join(cwd, ".cxt", "capture", "claude.baseline"))
	if err != nil || !strings.Contains(string(b), session) {
		t.Errorf("baseline not recorded: %v %s", err, b)
	}
}

func TestLockBlocksConcurrent(t *testing.T) {
	coord, fs, cwd, session := newTestCoord(t)
	ctx := context.Background()
	dir, err := captureStateDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "claude.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, true, false)
	if len(fs.calls) != 0 {
		t.Fatalf("fresh lock should block: %d saves", len(fs.calls))
	}
	// Stale lock (over 2 minutes) is ignored and captured.
	old := time.Now().Add(-3 * time.Minute)
	_ = os.Chtimes(lock, old, old)
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, true, false)
	if len(fs.calls) != 1 {
		t.Fatalf("stale lock takeover failed: %d saves", len(fs.calls))
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("lock not released after capture")
	}
}

func TestNoActiveSessionSilent(t *testing.T) {
	cwd := t.TempDir()
	_ = os.MkdirAll(filepath.Join(cwd, ".cxt"), 0o755)
	fs := &fakeSave{err: domain.ErrNoActiveSession}
	coord := NewCaptureCoordinator(fs, domain.TeamIdentity{})
	session := filepath.Join(t.TempDir(), "s.jsonl")
	_ = os.WriteFile(session, []byte("x\n"), 0o644)
	// Save returns ErrNoActiveSession (isolation gate, etc.) silently no-op.
	if _, err := coord.RequestCapture(context.Background(), domain.ProviderClaude, cwd, session, true, false); err != nil {
		t.Fatalf("expected silent no-op, got %v", err)
	}
}

// TestNotCxtRepoNoop sets the opt-in gate to fixed: in a repo without .cxt,
// it does not capture or create state files (to prevent any registered agent hooks from contaminating any repo).
func TestNotCxtRepoNoop(t *testing.T) {
	cwd := t.TempDir() // no .cxt
	fs := &fakeSave{}
	coord := NewCaptureCoordinator(fs, domain.TeamIdentity{})
	session := filepath.Join(t.TempDir(), "s.jsonl")
	_ = os.WriteFile(session, []byte("x\n"), 0o644)
	ctx := context.Background()
	if _, err := coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, false, true); err != nil {
		t.Fatalf("gate must be silent: %v", err)
	}
	_ = coord.MarkBaseline(ctx, domain.ProviderClaude, cwd, session)
	_ = coord.MarkTurn(ctx, domain.ProviderCodex, cwd, "hint")
	if len(fs.calls) != 0 {
		t.Fatal("non-cxt repo must not capture")
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
		t.Fatal("gate must not create .cxt in foreign repos")
	}
}
