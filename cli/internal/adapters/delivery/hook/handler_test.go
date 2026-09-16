package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type recSave struct{ calls []inbound.SaveInput }

func (r *recSave) Save(_ context.Context, in inbound.SaveInput) (inbound.SaveOutput, error) {
	r.calls = append(r.calls, in)
	return inbound.SaveOutput{SnapshotID: "sha256:x", Branch: "main"}, nil
}

func hookTestGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func initHookContext(t *testing.T, repo string) {
	t.Helper()
	hookTestGit(t, repo, "init", "-b", "main")
	if err := os.MkdirAll(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalAgentHookDoesNotCreateStoreInUnconnectedGitRepository(t *testing.T) {
	repo := t.TempDir()
	hookTestGit(t, repo, "init", "-b", "main")
	rs := &recSave{}
	h := NewHandler(capture.NewCaptureCoordinator(rs, domain.TeamIdentity{}))
	h.stdin = strings.NewReader(`{"session_id":"unconnected","cwd":"` + repo + `","prompt":"continue"}`)
	if err := h.Run(domain.ProviderCodex, "UserPromptSubmit"); err != nil {
		t.Fatal(err)
	}
	if len(rs.calls) != 0 {
		t.Fatalf("unconnected repository captured: %+v", rs.calls)
	}
	if _, err := os.Lstat(filepath.Join(repo, ".cxt")); !os.IsNotExist(err) {
		t.Fatalf("global agent hook created .cxt: %v", err)
	}
}

func TestGlobalAgentHookQuarantinesDirectoryOnlyResidue(t *testing.T) {
	repo := t.TempDir()
	hookTestGit(t, repo, "init", "-b", "main")
	residue := filepath.Join(repo, ".cxt", "capture")
	if err := os.MkdirAll(residue, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(residue, "legacy.turn")
	if err := os.WriteFile(legacy, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rs := &recSave{}
	h := NewHandler(capture.NewCaptureCoordinator(rs, domain.TeamIdentity{}))
	h.stdin = strings.NewReader(`{"session_id":"legacy-residue","cwd":"` + repo + `","prompt":"must not capture"}`)
	if err := h.Run(domain.ProviderCodex, "UserPromptSubmit"); err != nil {
		t.Fatal(err)
	}
	if len(rs.calls) != 0 {
		t.Fatalf("directory-only residue captured: %+v", rs.calls)
	}
	if _, err := os.Stat(filepath.Join(repo, ".cxt", "HEAD")); !os.IsNotExist(err) {
		t.Fatalf("hook promoted residue into an initialized store: %v", err)
	}
	if got, err := os.ReadFile(legacy); err != nil || string(got) != "keep me\n" {
		t.Fatalf("legacy residue was mutated: %q, %v", got, err)
	}
	ignore, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil || !strings.Contains(string(ignore), ".cxt/") {
		t.Fatalf(".gitignore was not repaired: %q, %v", ignore, err)
	}
	if got := hookTestGit(t, repo, "check-ignore", ".cxt/probe"); got == "" {
		t.Fatal("legacy .cxt residue remains visible to git")
	}
}

// TestHandlerStopWithPayload fixes the flow where Stop capture continues from the transcript path/cwd (capture path — detection omission path).
func TestHandlerStopWithPayload(t *testing.T) {
	cwd := t.TempDir()
	initHookContext(t, cwd)
	session := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(session, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rs := &recSave{}
	h := NewHandler(capture.NewCaptureCoordinator(rs, domain.TeamIdentity{}))
	h.stdin = strings.NewReader(`{"session_id":"s1","transcript_path":"` + session + `","cwd":"` + cwd + `"}`)
	if err := h.Run(domain.ProviderClaude, "Stop"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rs.calls) != 1 || rs.calls[0].SessionPath != session || rs.calls[0].Cwd != cwd {
		t.Fatalf("payload path/cwd not used: %+v", rs.calls)
	}
}

// TestHandlerPromptHint fixes the flow from UserPromptSubmit to the next capture message hint.
func TestHandlerPromptHint(t *testing.T) {
	cwd := t.TempDir()
	initHookContext(t, cwd)
	session := filepath.Join(t.TempDir(), "s.jsonl")
	_ = os.WriteFile(session, []byte("{}\n"), 0o644)
	rs := &recSave{}
	coord := capture.NewCaptureCoordinator(rs, domain.TeamIdentity{})
	h := NewHandler(coord)
	h.stdin = strings.NewReader(`{"cwd":"` + cwd + `","prompt":"refactor the loader"}`)
	if err := h.Run(domain.ProviderCodex, "UserPromptSubmit"); err != nil {
		t.Fatal(err)
	}
	h2 := NewHandler(coord)
	h2.stdin = strings.NewReader(`{"cwd":"` + cwd + `","rollout_path":"` + session + `"}`)
	if err := h2.Run(domain.ProviderCodex, "Stop"); err != nil {
		t.Fatal(err)
	}
	if len(rs.calls) != 1 || rs.calls[0].Message != domain.HookMessagePrefix+"refactor the loader" {
		t.Fatalf("prompt hint not propagated: %+v", rs.calls)
	}
}

// TestHandlerUnknownEventNoop fixes the fail-open behavior for unknown events.
func TestHandlerUnknownEventNoop(t *testing.T) {
	rs := &recSave{}
	h := NewHandler(capture.NewCaptureCoordinator(rs, domain.TeamIdentity{}))
	h.stdin = strings.NewReader(`{}`)
	if err := h.Run(domain.ProviderClaude, "SomeFutureEvent"); err != nil {
		t.Fatalf("unknown event must be noop: %v", err)
	}
	if len(rs.calls) != 0 {
		t.Fatal("unknown event must not capture")
	}
}

// TestHandlerBriefingEmission fixes the one-time consumption and additionalContext emission of pull briefing:
// The UserPromptSubmit hook consumes its scoped briefing queue and outputs hookSpecificOutput JSON to stdout (model transmission channel), and in the second utterance, nothing is output.
func TestHandlerBriefingEmission(t *testing.T) {
	cwd := t.TempDir()
	initHookContext(t, cwd)
	noticeID := domain.HashContent([]byte("handler pull briefing"))
	if err := capture.WritePullBriefing(cwd, "main", []domain.ContentHash{noticeID}); err != nil {
		t.Fatal(err)
	}
	rs := &recSave{}
	coord := capture.NewCaptureCoordinator(rs, domain.TeamIdentity{})

	var out1 bytes.Buffer
	h := NewHandler(coord)
	h.stdin = strings.NewReader(`{"cwd":"` + cwd + `","prompt":"continue"}`)
	h.stdout = &out1
	if err := h.Run(domain.ProviderClaude, "UserPromptSubmit"); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out1.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out1.String())
	}
	if decoded.HookSpecificOutput.HookEventName != "UserPromptSubmit" ||
		!strings.Contains(decoded.HookSpecificOutput.AdditionalContext, string(noticeID)) ||
		!strings.Contains(decoded.HookSpecificOutput.AdditionalContext, "identifiers only") {
		t.Fatalf("additionalContext emission error: %+v", decoded)
	}
	// 1st run: the second invocation is silent.
	var out2 bytes.Buffer
	h2 := NewHandler(coord)
	h2.stdin = strings.NewReader(`{"cwd":"` + cwd + `"}`)
	h2.stdout = &out2
	_ = h2.Run(domain.ProviderClaude, "UserPromptSubmit")
	if out2.Len() != 0 {
		t.Fatalf("briefing is resubscribed: %q", out2.String())
	}
}

func TestHandlerEmitsOnlyMatchingAppSessionHandoff(t *testing.T) {
	cwd := t.TempDir()
	initHookContext(t, cwd)
	const (
		first  = "11111111-1111-4111-8111-111111111111"
		second = "22222222-2222-4222-8222-222222222222"
	)
	if err := capture.WriteSessionHandoff(cwd, []string{first}, "FIRST APP BRANCH MEMORY"); err != nil {
		t.Fatal(err)
	}
	if err := capture.WriteSessionHandoff(cwd, []string{second}, "SECOND APP BRANCH MEMORY"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	h := NewHandler(capture.NewCaptureCoordinator(&recSave{}, domain.TeamIdentity{}))
	h.stdin = strings.NewReader(`{"session_id":"` + second + `","cwd":"` + cwd + `","prompt":"continue"}`)
	h.stdout = &out
	if err := h.Run(domain.ProviderCodex, "UserPromptSubmit"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "SECOND APP BRANCH MEMORY") || strings.Contains(out.String(), "FIRST APP BRANCH MEMORY") {
		t.Fatalf("wrong app handoff emitted: %s", out.String())
	}
	if got, ok := capture.ConsumeSessionHandoff(cwd, first); !ok || got != "FIRST APP BRANCH MEMORY" {
		t.Fatalf("other app session queue was consumed: %q, %v", got, ok)
	}
}

func TestHandlerTracksOpaqueAppSessionUntilSessionEnd(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	initHookContext(t, cwd)
	const hookSessionID = "codex-thread-opaque-id"
	const nativeID = "33333333-3333-4333-8333-333333333333"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "08", "31")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-08-31T00-00-00-"+nativeID+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rawPayload, err := json.Marshal(map[string]string{
		"session_id": hookSessionID, "transcript_path": path, "cwd": cwd, "prompt": "continue",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := string(rawPayload)
	h := NewHandler(capture.NewCaptureCoordinator(&recSave{}, domain.TeamIdentity{}))
	h.stdin = strings.NewReader(payload)
	if err := h.Run(domain.ProviderCodex, "UserPromptSubmit"); err != nil {
		t.Fatal(err)
	}
	if got := capture.ActiveAppSessions(cwd); len(got) != 1 || got[0].SessionID != hookSessionID || got[0].Path != path {
		t.Fatalf("tracked app sessions = %+v", got)
	}

	end := NewHandler(capture.NewCaptureCoordinator(&recSave{}, domain.TeamIdentity{}))
	end.stdin = strings.NewReader(payload)
	if err := end.Run(domain.ProviderCodex, "SessionEnd"); err != nil {
		t.Fatal(err)
	}
	if got := capture.ActiveAppSessions(cwd); len(got) != 0 {
		t.Fatalf("ended app session remains tracked: %+v", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("SessionEnd removed provider transcript: %v", err)
	}
}

// TestHandlerGarbagePayload ensures it doesn't die on non-JSON stdin ( best-effort).
func TestHandlerGarbagePayload(t *testing.T) {
	t.Chdir(t.TempDir())
	rs := &recSave{}
	h := NewHandler(capture.NewCaptureCoordinator(rs, domain.TeamIdentity{}))
	h.stdin = strings.NewReader("not json at all")
	// cwd fallback is os.Getwd — must be a no-op silently if there's no active session.
	if err := h.Run(domain.ProviderClaude, "SessionEnd"); err != nil {
		t.Fatalf("garbage payload must not error: %v", err)
	}
}

type hookSaveFunc func(context.Context, inbound.SaveInput) (inbound.SaveOutput, error)

func (f hookSaveFunc) Save(ctx context.Context, in inbound.SaveInput) (inbound.SaveOutput, error) {
	return f(ctx, in)
}

func TestSessionEndCapturesTailAfterConcurrentStop(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "wait-for-writer"
		if deadline {
			name = "deadline-retains-retry"
		}
		t.Run(name, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CXT_REMOTE", "")
			initHookContext(t, cwd)
			const sessionID = "opaque-final-session"
			session := filepath.Join(home, ".codex", "sessions", "2026", "09", "16", "rollout-test-11111111-1111-4111-8111-111111111111.jsonl")
			if err := os.MkdirAll(filepath.Dir(session), 0o755); err != nil {
				t.Fatal(err)
			}
			const initial = "{\"type\":\"user\"}\n"
			const tail = "{\"type\":\"assistant\",\"text\":\"final answer\"}\n"
			if err := os.WriteFile(session, []byte(initial), 0o600); err != nil {
				t.Fatal(err)
			}
			firstRead, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			var mu sync.Mutex
			var snapshots []string
			saver := hookSaveFunc(func(ctx context.Context, in inbound.SaveInput) (inbound.SaveOutput, error) {
				raw, err := os.ReadFile(in.SessionPath)
				if err != nil {
					return inbound.SaveOutput{}, err
				}
				mu.Lock()
				snapshots = append(snapshots, string(raw))
				first := len(snapshots) == 1
				mu.Unlock()
				if first {
					close(firstRead)
					select {
					case <-release:
					case <-ctx.Done():
						return inbound.SaveOutput{}, ctx.Err()
					}
				}
				return inbound.SaveOutput{SnapshotID: "sha256:tail", Branch: "main"}, nil
			})
			newHandler := func(path string) *Handler {
				payload, err := json.Marshal(hookPayload{SessionID: sessionID, RolloutPath: path, Cwd: cwd})
				if err != nil {
					t.Fatal(err)
				}
				// Separate coordinators model separate hook processes sharing only disk state.
				h := NewHandler(capture.NewCaptureCoordinator(saver, domain.TeamIdentity{}))
				h.stdin = bytes.NewReader(payload)
				return h
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			var wg sync.WaitGroup
			t.Cleanup(func() { cancel(); unblock(); wg.Wait() })
			stopDone := make(chan error, 1)
			stop := newHandler(session)
			wg.Go(func() { stopDone <- stop.run(ctx, domain.ProviderCodex, "Stop") })
			select {
			case <-firstRead:
			case <-ctx.Done():
				t.Fatal("Stop did not read its initial snapshot")
			}
			// Append only after the older capture read its bytes, while it still owns the lock.
			f, err := os.OpenFile(session, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString(tail)
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("append tail: %v, close: %v", err, closeErr)
			}
			endCtx := ctx
			if deadline {
				var endCancel context.CancelFunc
				endCtx, endCancel = context.WithTimeout(ctx, 150*time.Millisecond)
				defer endCancel()
			}
			endDone := make(chan error, 1)
			end := newHandler("") // a pathless final hook must retain the opaque ID's exact binding
			wg.Go(func() { endDone <- end.run(endCtx, domain.ProviderCodex, "SessionEnd") })
			if deadline {
				if err := <-endDone; !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("contended final capture must report deadline, got %v", err)
				}
			} else {
				select {
				case err := <-endDone:
					t.Fatalf("SessionEnd returned before the older writer released its lock: %v", err)
				case <-time.After(150 * time.Millisecond):
				}
			}
			if active := capture.ActiveAppSessions(cwd); len(active) != 1 || active[0].SessionID != sessionID || active[0].Path != session {
				t.Fatalf("final capture lost its durable retry pointer: %+v", active)
			}
			unblock()
			if err := <-stopDone; err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if deadline {
				// A new handler/coordinator recovers the path entirely from disk.
				if err := newHandler("").run(ctx, domain.ProviderCodex, "SessionEnd"); err != nil {
					t.Fatalf("retry: %v", err)
				}
			} else if err := <-endDone; err != nil {
				t.Fatalf("SessionEnd: %v", err)
			}
			mu.Lock()
			got := append([]string(nil), snapshots...)
			mu.Unlock()
			if len(got) != 2 || got[0] != initial || got[1] != initial+tail {
				t.Fatalf("final tail was not captured: %q", got)
			}
			if active := capture.ActiveAppSessions(cwd); len(active) != 0 {
				t.Fatalf("successful final capture left liveness behind: %+v", active)
			}
			if raw, err := os.ReadFile(session); err != nil || string(raw) != initial+tail {
				t.Fatalf("capture changed provider transcript: %q, %v", raw, err)
			}
		})
	}
}

func TestSessionEndSaveFailureRetainsNewlyResolvedSession(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CXT_REMOTE", "")
	initHookContext(t, cwd)
	const sessionID = "11111111-1111-4111-8111-111111111111"
	session := filepath.Join(home, ".codex", "sessions", "2026", "09", "16", "rollout-test-"+sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(session), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"cwd": cwd}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(hookPayload{SessionID: sessionID, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("save unavailable")
	fail := hookSaveFunc(func(_ context.Context, in inbound.SaveInput) (inbound.SaveOutput, error) {
		if in.SessionPath != session {
			t.Fatalf("wrong final transcript: %q", in.SessionPath)
		}
		return inbound.SaveOutput{}, wantErr
	})
	h := NewHandler(capture.NewCaptureCoordinator(fail, domain.TeamIdentity{}))
	h.stdin = bytes.NewReader(payload)
	if err := h.Run(domain.ProviderCodex, "SessionEnd"); !errors.Is(err, wantErr) {
		t.Fatalf("final capture error = %v, want %v", err, wantErr)
	}
	if active := capture.ActiveAppSessions(cwd); len(active) != 1 || active[0].SessionID != sessionID || active[0].Path != session {
		t.Fatalf("failed final capture did not retain its resolved transcript: %+v", active)
	}
	saved := &recSave{}
	retry := NewHandler(capture.NewCaptureCoordinator(saved, domain.TeamIdentity{}))
	retry.stdin = bytes.NewReader(payload)
	if err := retry.Run(domain.ProviderCodex, "SessionEnd"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(saved.calls) != 1 || saved.calls[0].SessionPath != session {
		t.Fatalf("retry calls = %+v", saved.calls)
	}
	if active := capture.ActiveAppSessions(cwd); len(active) != 0 {
		t.Fatalf("successful retry left liveness behind: %+v", active)
	}
}
