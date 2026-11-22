package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

// hookSummaries gives one text, like the service that reads the branch's
// summary, and records what emit answered.
type hookSummaries struct {
	inputs   []inbound.BranchSummaryInput
	text     string
	err      error
	emitted  []error
	deadline bool
}

func (f *hookSummaries) DeliverBranchSummary(ctx context.Context, in inbound.BranchSummaryInput, emit func(string) error) error {
	f.inputs = append(f.inputs, in)
	_, f.deadline = ctx.Deadline()
	if f.err != nil || f.text == "" {
		return f.err
	}
	err := emit(f.text)
	f.emitted = append(f.emitted, err)
	return err
}

// The summary along the session's branch is what a hook gives the tool. A
// request the checkout hook queued is dropped, and the server's main memory
// is not prepared.
func TestHandlerGivesTheBranchSummaryInPlaceOfTheQueuedHandoff(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderCodex, domain.ProviderClaude} {
		for _, event := range []string{"SessionStart", "UserPromptSubmit"} {
			t.Run(string(provider)+"/"+event, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				cwd := t.TempDir()
				initHookContext(t, cwd)
				const session = "11111111-1111-4111-8111-111111111111"
				path := writeHookSession(t, cwd, provider, session)
				if err := capture.WriteSessionHandoff(cwd, []string{session}, "OLD QUEUED PRIVATE BODY"); err != nil {
					t.Fatal(err)
				}
				if err := capture.WriteSessionHandoff(cwd, []string{"another-session"}, "OTHER SESSION BODY"); err != nil {
					t.Fatal(err)
				}
				prepared := &hookHandoffPreparer{text: "SERVER MAIN MEMORY"}
				summaries := &hookSummaries{text: "SUMMARY ALONG THE BRANCH"}
				var output bytes.Buffer
				h := NewHandler(capture.NewCaptureCoordinator(&recSave{}, domain.TeamIdentity{})).WithAgentContext(prepared).WithBranchSummaries(summaries)
				payload, _ := json.Marshal(hookPayload{Cwd: cwd, SessionID: session, TranscriptPath: path, Prompt: "continue"})
				h.stdin, h.stdout = bytes.NewReader(payload), &output
				if err := h.Run(provider, event); err != nil {
					t.Fatal(err)
				}
				if text := handoffContext(t, output.Bytes(), event); text != "SUMMARY ALONG THE BRANCH" {
					t.Fatalf("what the tool was given: %s", text)
				}
				want := []inbound.BranchSummaryInput{{Cwd: cwd, Provider: provider, SessionID: session}}
				if !reflect.DeepEqual(summaries.inputs, want) || !summaries.deadline || !reflect.DeepEqual(summaries.emitted, []error{nil}) || len(prepared.inputs) != 0 {
					t.Fatalf("what was asked: %+v deadline=%v emitted=%v prepared=%v", summaries.inputs, summaries.deadline, summaries.emitted, prepared.inputs)
				}
				if _, queued := capture.ConsumeSessionHandoff(cwd, session); queued {
					t.Fatal("the queued request of the session was kept")
				}
				if text, ok := capture.ConsumeSessionHandoff(cwd, "another-session"); !ok || text != "OTHER SESSION BODY" {
					t.Fatal("the queue of another session was consumed")
				}

				// Nothing to give: the hook writes nothing.
				summaries.text = ""
				output.Reset()
				h.stdin = bytes.NewReader(payload)
				if err := h.Run(provider, event); err != nil || output.Len() != 0 || len(summaries.inputs) != 2 {
					t.Fatalf("nothing to give: %v %q", err, output.String())
				}
			})
		}
	}
}

// A summary that could not be read, or did not reach the tool, is reported
// and left to the service to try again.
func TestHandlerReportsABranchSummaryThatWasNotGiven(t *testing.T) {
	offline, closed := errors.New("server unreachable"), errors.New("output closed")
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	initHookContext(t, cwd)
	const session = "22222222-2222-4222-8222-222222222222"
	path := writeHookSession(t, cwd, domain.ProviderClaude, session)
	payload, _ := json.Marshal(hookPayload{Cwd: cwd, SessionID: session, TranscriptPath: path, Prompt: "continue"})

	summaries := &hookSummaries{err: offline}
	var output bytes.Buffer
	h := NewHandler(capture.NewCaptureCoordinator(&recSave{}, domain.TeamIdentity{})).WithBranchSummaries(summaries)
	h.stdin, h.stdout = bytes.NewReader(payload), &output
	if err := h.Run(domain.ProviderClaude, "UserPromptSubmit"); !errors.Is(err, offline) || output.Len() != 0 {
		t.Fatalf("an unreachable server: %v %q", err, output.String())
	}

	summaries.err, summaries.text = nil, "SUMMARY ALONG THE BRANCH"
	h.stdin, h.stdout = bytes.NewReader(payload), hookHandoffWriter(func([]byte) (int, error) { return 0, closed })
	if err := h.Run(domain.ProviderClaude, "UserPromptSubmit"); !errors.Is(err, closed) || len(summaries.emitted) != 1 || !errors.Is(summaries.emitted[0], closed) {
		t.Fatalf("a text that did not reach the tool: %v %v", err, summaries.emitted)
	}
	// A short write is not a text that reached the tool either.
	h.stdin, h.stdout = bytes.NewReader(payload), hookHandoffWriter(func(raw []byte) (int, error) { return len(raw) - 1, nil })
	if err := h.Run(domain.ProviderClaude, "UserPromptSubmit"); err == nil || len(summaries.emitted) != 2 || summaries.emitted[1] == nil {
		t.Fatalf("a short write: %v %v", err, summaries.emitted)
	}
	if strings.Contains(output.String(), "SUMMARY") {
		t.Fatalf("output: %q", output.String())
	}
}
