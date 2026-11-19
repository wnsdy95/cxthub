package hook

import (
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// An agent hook only wakes the session report collector: once for each
// handled event in a repository cxt is set up in, and never elsewhere.
func TestAgentHooksWakeTheSegmentCollector(t *testing.T) {
	cwd := t.TempDir()
	initHookContext(t, cwd)
	t.Setenv("HOME", t.TempDir())
	const id = "22222222-2222-4222-8222-222222222222"
	session := writeHookSession(t, cwd, domain.ProviderClaude, id)
	var woken []string
	handler := func() *Handler {
		h := NewHandler(capture.NewCaptureCoordinator(&recSave{}, domain.TeamIdentity{})).WithSegmentCollector(func(dir string) { woken = append(woken, dir) })
		h.stdin = strings.NewReader(`{"session_id":"` + id + `","transcript_path":"` + session + `","cwd":"` + cwd + `","prompt":"continue"}`)
		return h
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		woken = nil
		if err := handler().Run(domain.ProviderClaude, event); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		if len(woken) != 1 || woken[0] != cwd {
			t.Fatalf("%s woke %v", event, woken)
		}
	}
	// An event the handler does not handle wakes nothing.
	woken = nil
	if err := handler().Run(domain.ProviderClaude, "PreToolUse"); err != nil || len(woken) != 0 {
		t.Fatalf("an unhandled event: %v %v", woken, err)
	}
	// A repository cxt is not set up in is left alone.
	other := t.TempDir()
	hookTestGit(t, other, "init", "-b", "main")
	h := NewHandler(capture.NewCaptureCoordinator(&recSave{}, domain.TeamIdentity{})).WithSegmentCollector(func(dir string) { woken = append(woken, dir) })
	h.stdin = strings.NewReader(`{"session_id":"unconnected","cwd":"` + other + `","prompt":"continue"}`)
	if err := h.Run(domain.ProviderCodex, "UserPromptSubmit"); err != nil || len(woken) != 0 {
		t.Fatalf("an unconnected repository: %v %v", woken, err)
	}
}
