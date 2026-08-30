package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type recSave struct{ calls []inbound.SaveInput }

func (r *recSave) Save(_ context.Context, in inbound.SaveInput) (inbound.SaveOutput, error) {
	r.calls = append(r.calls, in)
	return inbound.SaveOutput{SnapshotID: "sha256:x", Branch: "main"}, nil
}

// TestHandlerStopWithPayload fixes the flow where Stop capture continues from the transcript path/cwd (capture path — detection omission path).
func TestHandlerStopWithPayload(t *testing.T) {
	cwd := t.TempDir()
	_ = os.MkdirAll(filepath.Join(cwd, ".cxt"), 0o755)
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
	_ = os.MkdirAll(filepath.Join(cwd, ".cxt"), 0o755)
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
	_ = os.MkdirAll(filepath.Join(cwd, ".cxt"), 0o755)
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
	_ = os.MkdirAll(filepath.Join(cwd, ".cxt"), 0o755)
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

// TestHandlerGarbagePayload ensures it doesn't die on non-JSON stdin ( best-effort).
func TestHandlerGarbagePayload(t *testing.T) {
	rs := &recSave{}
	h := NewHandler(capture.NewCaptureCoordinator(rs, domain.TeamIdentity{}))
	h.stdin = strings.NewReader("not json at all")
	// cwd fallback is os.Getwd — must be a no-op silently if there's no active session.
	if err := h.Run(domain.ProviderClaude, "SessionEnd"); err != nil {
		t.Fatalf("garbage payload must not error: %v", err)
	}
}
