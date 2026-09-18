package capture

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func captureGitRun(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

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
	if _, err := coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, "", true, false); err != nil {
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
	base := captureStateBase(ctx, domain.ProviderClaude, cwd, "")
	for _, ext := range []string{".last", ".cursor"} {
		if _, err := os.Stat(filepath.Join(cwd, ".cxt", "capture", base+ext)); err != nil {
			t.Errorf("state file %s missing: %v", ext, err)
		}
	}
}

func TestRequestCaptureDebounce(t *testing.T) {
	coord, fs, cwd, session := newTestCoord(t)
	ctx := context.Background()
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, "", true, false)
	// Skip if file shrank but not inside Windows.
	_ = os.WriteFile(session, []byte("{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n"), 0o644)
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, "", true, false)
	if len(fs.calls) != 1 {
		t.Fatalf("debounce failed: %d saves", len(fs.calls))
	}
	// Recapture if outside Windows (.last mtime modified in the past).
	last := filepath.Join(cwd, ".cxt", "capture", captureStateBase(ctx, domain.ProviderClaude, cwd, "")+".last")
	old := time.Now().Add(-2 * time.Minute)
	_ = os.Chtimes(last, old, old)
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, "", true, false)
	if len(fs.calls) != 2 {
		t.Fatalf("post-window capture failed: %d saves", len(fs.calls))
	}
}

func TestRequestCaptureGrowthGate(t *testing.T) {
	coord, fs, cwd, session := newTestCoord(t)
	ctx := context.Background()
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, "", false, true)
	// No-op if file didn't shrink (content unchanged → dedup target, I/O savings).
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, "", false, true)
	if len(fs.calls) != 1 {
		t.Fatalf("growth gate failed: %d saves", len(fs.calls))
	}
	// If true, force ignores debouncing and captures immediately.
	_ = os.WriteFile(session, []byte("{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n"), 0o644)
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, "", false, true)
	if len(fs.calls) != 2 {
		t.Fatalf("force flush failed: %d saves", len(fs.calls))
	}
}

func TestTurnHintConsumed(t *testing.T) {
	coord, fs, cwd, session := newTestCoord(t)
	ctx := context.Background()
	if err := coord.MarkTurn(ctx, domain.ProviderCodex, cwd, "", "fix the parser bug\nsecond line ignored"); err != nil {
		t.Fatal(err)
	}
	_, _ = coord.RequestCapture(ctx, domain.ProviderCodex, cwd, session, "", true, false)
	if len(fs.calls) != 1 || fs.calls[0].Message != domain.HookMessagePrefix+"fix the parser bug" {
		t.Fatalf("turn hint not used: %+v", fs.calls)
	}
	turn := captureStateBase(ctx, domain.ProviderCodex, cwd, "") + ".turn"
	if _, err := os.Stat(filepath.Join(cwd, ".cxt", "capture", turn)); !os.IsNotExist(err) {
		t.Error("turn hint not consumed (file remains)")
	}
}

func TestMarkBaselineClearsTurn(t *testing.T) {
	coord, _, cwd, session := newTestCoord(t)
	ctx := context.Background()
	_ = coord.MarkTurn(ctx, domain.ProviderClaude, cwd, "", "stale hint")
	if err := coord.MarkBaseline(ctx, domain.ProviderClaude, cwd, session, ""); err != nil {
		t.Fatal(err)
	}
	base := captureStateBase(ctx, domain.ProviderClaude, cwd, "")
	if _, err := os.Stat(filepath.Join(cwd, ".cxt", "capture", base+".turn")); !os.IsNotExist(err) {
		t.Error("stale turn hint not cleared at session boundary")
	}
	b, err := os.ReadFile(filepath.Join(cwd, ".cxt", "capture", base+".baseline"))
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
	lock := filepath.Join(dir, captureStateBase(ctx, domain.ProviderClaude, cwd, "")+".lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, "", true, false)
	if len(fs.calls) != 0 {
		t.Fatalf("fresh lock should block: %d saves", len(fs.calls))
	}
	// Stale lock (over 2 minutes) is ignored and captured.
	old := time.Now().Add(-3 * time.Minute)
	_ = os.Chtimes(lock, old, old)
	_, _ = coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, "", true, false)
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
	if _, err := coord.RequestCapture(context.Background(), domain.ProviderClaude, cwd, session, "", true, false); err != nil {
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
	if _, err := coord.RequestCapture(ctx, domain.ProviderClaude, cwd, session, "", false, true); err != nil {
		t.Fatalf("gate must be silent: %v", err)
	}
	_ = coord.MarkBaseline(ctx, domain.ProviderClaude, cwd, session, "")
	_ = coord.MarkTurn(ctx, domain.ProviderCodex, cwd, "", "hint")
	if len(fs.calls) != 0 {
		t.Fatal("non-cxt repo must not capture")
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
		t.Fatal("gate must not create .cxt in foreign repos")
	}
}

func TestLinkedWorktreeCaptureUsesSharedStore(t *testing.T) {
	primary := filepath.Join(t.TempDir(), "primary")
	linked := filepath.Join(t.TempDir(), "desktop-worktree")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	captureGitRun(t, primary, "init", "-b", "main")
	captureGitRun(t, primary, "config", "user.name", "cxt test")
	captureGitRun(t, primary, "config", "user.email", "cxt@example.test")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	captureGitRun(t, primary, "config", "core.hooksPath", hooks)
	captureGitRun(t, primary, "config", "gc.auto", "0")
	if err := os.WriteFile(filepath.Join(primary, "tracked.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureGitRun(t, primary, "add", "tracked.txt")
	captureGitRun(t, primary, "commit", "-m", "initial")
	if err := os.Mkdir(filepath.Join(primary, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primary, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureGitRun(t, primary, "worktree", "add", "-b", "app/session", linked)
	primary, _ = filepath.EvalSymlinks(primary)
	linked, _ = filepath.EvalSymlinks(linked)

	session := filepath.Join(t.TempDir(), "app-session.jsonl")
	if err := os.WriteFile(session, []byte("{\"type\":\"user\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := &fakeSave{}
	coord := NewCaptureCoordinator(fs, domain.TeamIdentity{})
	ctx := context.Background()
	const sessionID = "desktop-session-one"
	briefingID := domain.HashContent([]byte("desktop worktree briefing"))
	if err := WritePullBriefing(linked, "app/session", []domain.ContentHash{briefingID}); err != nil {
		t.Fatal(err)
	}
	if text, ok := ConsumeBriefing(linked); !ok || !strings.Contains(text, string(briefingID)) {
		t.Fatalf("linked worktree briefing was not delivered from shared store: ok=%v text=%q", ok, text)
	}
	if captured, err := coord.RequestCapture(ctx, domain.ProviderCodex, linked, session, sessionID, false, true); err != nil || !captured {
		t.Fatalf("linked capture captured=%v err=%v", captured, err)
	}
	if len(fs.calls) != 1 || fs.calls[0].Cwd != linked || fs.calls[0].SessionPath != session {
		t.Fatalf("worktree identity was not preserved for save: %+v", fs.calls)
	}
	base := captureStateBase(ctx, domain.ProviderCodex, linked, sessionID)
	if _, err := os.Stat(filepath.Join(primary, ".cxt", "capture", base+".cursor")); err != nil {
		t.Fatalf("shared capture cursor missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(linked, ".cxt")); !os.IsNotExist(err) {
		t.Fatalf("linked worktree acquired a split .cxt store: %v", err)
	}
}

func TestConcurrentAppSessionsHaveIndependentCaptureState(t *testing.T) {
	coord, fs, cwd, first := newTestCoord(t)
	second := filepath.Join(t.TempDir(), "second.jsonl")
	if err := os.WriteFile(second, []byte("{\"type\":\"user\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := coord.MarkTurn(ctx, domain.ProviderCodex, cwd, "app-session-a", "first app prompt"); err != nil {
		t.Fatal(err)
	}
	if err := coord.MarkTurn(ctx, domain.ProviderCodex, cwd, "app-session-b", "second app prompt"); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.RequestCapture(ctx, domain.ProviderCodex, cwd, first, "app-session-a", false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.RequestCapture(ctx, domain.ProviderCodex, cwd, second, "app-session-b", false, true); err != nil {
		t.Fatal(err)
	}
	if len(fs.calls) != 2 || fs.calls[0].Message != domain.HookMessagePrefix+"first app prompt" || fs.calls[1].Message != domain.HookMessagePrefix+"second app prompt" {
		t.Fatalf("app session state crossed: %+v", fs.calls)
	}
	firstBase := captureStateBase(ctx, domain.ProviderCodex, cwd, "app-session-a")
	secondBase := captureStateBase(ctx, domain.ProviderCodex, cwd, "app-session-b")
	if firstBase == secondBase {
		t.Fatal("distinct app sessions share one capture state prefix")
	}
}

func TestSameOpaqueSessionIDIsIndependentAcrossLinkedWorktrees(t *testing.T) {
	primary := filepath.Join(t.TempDir(), "primary")
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	captureGitRun(t, primary, "init", "-b", "main")
	captureGitRun(t, primary, "config", "user.name", "cxt test")
	captureGitRun(t, primary, "config", "user.email", "cxt@example.test")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	captureGitRun(t, primary, "config", "core.hooksPath", hooks)
	captureGitRun(t, primary, "config", "gc.auto", "0")
	captureGitRun(t, primary, "commit", "--allow-empty", "-m", "initial")
	captureGitRun(t, primary, "worktree", "add", "-b", "feature/app", linked)

	const opaqueID = "provider-may-reuse-this-id"
	ctx := context.Background()
	primaryBase := captureStateBase(ctx, domain.ProviderCodex, primary, opaqueID)
	linkedBase := captureStateBase(ctx, domain.ProviderCodex, linked, opaqueID)
	if primaryBase == linkedBase {
		t.Fatalf("same opaque ID crossed worktrees: %q", primaryBase)
	}
}

func writeCoordinatorSession(t *testing.T, home, cwd string, provider domain.ProviderKind, sessionID string, modified time.Time) string {
	t.Helper()
	path := filepath.Join(home, ".claude", "projects", encodeCwd(cwd), sessionID+".jsonl")
	if provider == domain.ProviderCodex {
		path = filepath.Join(home, ".codex", "sessions", "2026", "09", "16", "rollout-test-"+sessionID+".jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": sessionID, "cwd": cwd}, "sessionId": sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCoordinatorDiscoveryHonorsSessionIdentity(t *testing.T) {
	const wantedID = "11111111-1111-4111-8111-111111111111"
	const siblingID = "22222222-2222-4222-8222-222222222222"
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, scenario := range []string{"exact", "missing", "opaque-unresolved", "opaque-registered", "no-id"} {
			t.Run(string(provider)+"/"+scenario, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				coord, fs, cwd, _ := newTestCoord(t)
				wanted := writeCoordinatorSession(t, home, cwd, provider, wantedID, time.Unix(1000, 0))
				sibling := writeCoordinatorSession(t, home, cwd, provider, siblingID, time.Unix(2000, 0))
				sessionID, expected := wantedID, wanted
				switch scenario {
				case "missing":
					sessionID = "33333333-3333-4333-8333-333333333333"
				case "opaque-unresolved":
					sessionID = "opaque-hook-id"
				case "opaque-registered":
					sessionID = "opaque-hook-id"
					if err := TrackAppSession(cwd, provider, sessionID, wanted); err != nil {
						t.Fatal(err)
					}
				case "no-id":
					sessionID, expected = "", sibling
				}
				ctx := context.Background()
				baselineErr := coord.MarkBaseline(ctx, provider, cwd, "", sessionID)
				captured, captureErr := coord.RequestCapture(ctx, provider, cwd, "", sessionID, false, true)
				base := filepath.Join(cwd, ".cxt", "capture", captureStateBase(ctx, provider, cwd, sessionID))
				if scenario == "missing" || scenario == "opaque-unresolved" {
					for _, err := range []error{baselineErr, captureErr} {
						if !errors.Is(err, domain.ErrNoActiveSession) || !strings.Contains(err.Error(), sessionID) {
							t.Fatalf("unresolved identity must be explicit, got %v", err)
						}
					}
					if captured || len(fs.calls) != 0 {
						t.Fatalf("unresolved identity captured a sibling: captured=%v calls=%+v", captured, fs.calls)
					}
					for _, suffix := range []string{".baseline", ".cursor", ".last"} {
						if _, err := os.Stat(base + suffix); !os.IsNotExist(err) {
							t.Fatalf("unresolved identity advanced %s: %v", suffix, err)
						}
					}
					return
				}
				if baselineErr != nil || captureErr != nil || !captured {
					t.Fatalf("baseline=%v capture=%v captured=%v", baselineErr, captureErr, captured)
				}
				if len(fs.calls) != 1 || fs.calls[0].SessionPath != expected {
					t.Fatalf("capture calls=%+v, want path %q", fs.calls, expected)
				}
				for _, suffix := range []string{".baseline", ".cursor"} {
					data, err := os.ReadFile(base + suffix)
					var state captureCursor
					if err != nil || json.Unmarshal(data, &state) != nil || state.Path != expected {
						t.Fatalf("wrong %s: %s (read error: %v)", suffix, data, err)
					}
				}
			})
		}
	}
}

func TestForcedCaptureLockHonorsDeadlineAndCanRetry(t *testing.T) {
	coord, fs, cwd, session := newTestCoord(t)
	dir, err := captureStateDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "final-session"
	lock := filepath.Join(dir, captureStateBase(context.Background(), domain.ProviderCodex, cwd, sessionID)+".lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	captured, err := coord.RequestCapture(ctx, domain.ProviderCodex, cwd, session, sessionID, false, true)
	if captured || !errors.Is(err, context.DeadlineExceeded) || len(fs.calls) != 0 {
		t.Fatalf("blocked force capture: captured=%v err=%v calls=%+v", captured, err, fs.calls)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatalf("waiting capture removed another writer's lock: %v", err)
	}
	retry := NewCaptureCoordinator(fs, domain.TeamIdentity{})
	if captured, err := retry.RequestCapture(context.Background(), domain.ProviderCodex, cwd, session, sessionID, false, true); err != nil || !captured {
		t.Fatalf("retry: captured=%v err=%v", captured, err)
	}
}

func TestCaptureGateAllowsRewriteAndPolicyChange(t *testing.T) {
	coord, save, cwd, path := newTestCoord(t)
	ctx := context.Background()
	if _, err := coord.RequestCapture(ctx, domain.ProviderClaude, cwd, path, "", false, false); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Same length plus a different write time is not an unchanged transcript.
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Second)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.RequestCapture(ctx, domain.ProviderClaude, cwd, path, "", false, false); err != nil {
		t.Fatal(err)
	}
	if len(save.calls) != 2 {
		t.Fatal("same-size rewrite skipped")
	}
	if err := os.WriteFile(cwd+"/.cxtsecrets", []byte("new-secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.RequestCapture(ctx, domain.ProviderClaude, cwd, path, "", false, false); err != nil {
		t.Fatal(err)
	}
	if len(save.calls) != 3 {
		t.Fatal("new masking policy skipped")
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.RequestCapture(ctx, domain.ProviderClaude, cwd, path, "", false, false); err != nil {
		t.Fatal(err)
	}
	if len(save.calls) != 4 {
		t.Fatal("truncation skipped")
	}
}
