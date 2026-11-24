package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

// gitRepository makes a Git repository. With setUp it carries the mark that
// `cxt setup` writes.
func gitRepository(t *testing.T, setUp bool) string {
	t.Helper()
	repo := t.TempDir()
	cmd := exec.Command("git", "-C", repo, "init", "-q", "-b", "main")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if setUp {
		if _, err := gitctx.NewSetupMarker().MarkSetUp(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

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

type hookWriter func([]byte) (int, error)

func (f hookWriter) Write(raw []byte) (int, error) { return f(raw) }

// givenContext reads what the hook wrote for the tool.
func givenContext(t *testing.T, raw []byte, event string) string {
	t.Helper()
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.HookSpecificOutput.HookEventName != event {
		t.Fatalf("what the hook wrote for %s: %q (%v)", event, raw, err)
	}
	return out.HookSpecificOutput.AdditionalContext
}

func run(t *testing.T, h *Handler, provider domain.ProviderKind, event string, payload hookPayload, out *bytes.Buffer) error {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	h.stdin = bytes.NewReader(raw)
	if out != nil {
		h.stdout = out
	}
	return h.Run(provider, event)
}

// The summary along the session's branch is what a hook gives the tool, at
// the start of a session and at each prompt.
func TestHandlerGivesTheBranchSummary(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderCodex, domain.ProviderClaude} {
		for _, event := range []string{"SessionStart", "UserPromptSubmit"} {
			t.Run(string(provider)+"/"+event, func(t *testing.T) {
				cwd := gitRepository(t, true)
				const session = "11111111-1111-4111-8111-111111111111"
				summaries := &hookSummaries{text: "SUMMARY ALONG THE BRANCH"}
				var output bytes.Buffer
				h := NewHandler().WithBranchSummaries(summaries)
				payload := hookPayload{Cwd: cwd, SessionID: session}
				if err := run(t, h, provider, event, payload, &output); err != nil {
					t.Fatal(err)
				}
				if text := givenContext(t, output.Bytes(), event); text != "SUMMARY ALONG THE BRANCH" {
					t.Fatalf("what the tool was given: %s", text)
				}
				want := []inbound.BranchSummaryInput{{Cwd: cwd, Provider: provider, SessionID: session}}
				if !reflect.DeepEqual(summaries.inputs, want) || !summaries.deadline || !reflect.DeepEqual(summaries.emitted, []error{nil}) {
					t.Fatalf("what was asked: %+v deadline=%v emitted=%v", summaries.inputs, summaries.deadline, summaries.emitted)
				}

				// Nothing to give: the hook writes nothing.
				summaries.text = ""
				output.Reset()
				if err := run(t, h, provider, event, payload, &output); err != nil || output.Len() != 0 || len(summaries.inputs) != 2 {
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
	cwd := gitRepository(t, true)
	payload := hookPayload{Cwd: cwd, SessionID: "22222222-2222-4222-8222-222222222222"}

	summaries := &hookSummaries{err: offline}
	var output bytes.Buffer
	h := NewHandler().WithBranchSummaries(summaries)
	if err := run(t, h, domain.ProviderClaude, "UserPromptSubmit", payload, &output); !errors.Is(err, offline) || output.Len() != 0 {
		t.Fatalf("an unreachable server: %v %q", err, output.String())
	}

	summaries.err, summaries.text = nil, "SUMMARY ALONG THE BRANCH"
	h.stdout = hookWriter(func([]byte) (int, error) { return 0, closed })
	if err := run(t, h, domain.ProviderClaude, "UserPromptSubmit", payload, nil); !errors.Is(err, closed) || len(summaries.emitted) != 1 || !errors.Is(summaries.emitted[0], closed) {
		t.Fatalf("a text that did not reach the tool: %v %v", err, summaries.emitted)
	}
	// A short write is not a text that reached the tool either.
	h.stdout = hookWriter(func(raw []byte) (int, error) { return len(raw) - 1, nil })
	if err := run(t, h, domain.ProviderClaude, "UserPromptSubmit", payload, nil); err == nil || len(summaries.emitted) != 2 || summaries.emitted[1] == nil {
		t.Fatalf("a short write: %v %v", err, summaries.emitted)
	}
	if strings.Contains(output.String(), "SUMMARY") {
		t.Fatalf("output: %q", output.String())
	}
}

// The end of a turn and the end of a session wake the collector: a commit and
// a push usually happen in the middle of a turn, and the rest of the turn is
// recorded after the Git hooks ran. The start of a session and a prompt give
// the summary and wake nothing: nothing was recorded since the last turn
// ended. A hook is only what wakes the collector, which reads the tool's own
// records.
func TestTheEndOfATurnWakesTheCollector(t *testing.T) {
	cwd := gitRepository(t, true)
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
			var woken []string
			summaries := &hookSummaries{err: errors.New("server unreachable")}
			h := NewHandler().WithBranchSummaries(summaries).WithSegmentCollector(func(dir string) { woken = append(woken, dir) })
			err := run(t, h, provider, event, hookPayload{Cwd: cwd, SessionID: "33333333-3333-4333-8333-333333333333"}, &bytes.Buffer{})
			// Only the start of a session and a prompt ask for the summary;
			// only the two ends wake the collector, once, for the session's folder.
			asks := event == "SessionStart" || event == "UserPromptSubmit"
			want := []string{cwd}
			if asks {
				want = nil
			}
			if !reflect.DeepEqual(woken, want) {
				t.Fatalf("%s %s woke %v, want %v", provider, event, woken, want)
			}
			if (len(summaries.inputs) == 1) != asks || (err != nil) != asks {
				t.Fatalf("%s %s asked %d times and returned %v", provider, event, len(summaries.inputs), err)
			}
		}
	}
	// An event cxt does not handle wakes nothing.
	var woken []string
	h := NewHandler().WithSegmentCollector(func(dir string) { woken = append(woken, dir) })
	if err := run(t, h, domain.ProviderClaude, "PreToolUse", hookPayload{Cwd: cwd}, &bytes.Buffer{}); err != nil || len(woken) != 0 {
		t.Fatalf("an unhandled event: %v %v", err, woken)
	}
}

// The tools' hooks are registered for every folder. cxt acts only where
// `cxt setup` or `cxt init` wrote its mark.
func TestHandlerActsOnlyWhereCxtWasSetUp(t *testing.T) {
	notSetUp := gitRepository(t, false)
	residue := gitRepository(t, false)
	if err := os.Mkdir(filepath.Join(residue, ".cxt"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	for name, cwd := range map[string]string{"a repository that was not set up": notSetUp, "a bare .cxt directory": residue, "a folder outside Git": outside} {
		var woken []string
		summaries := &hookSummaries{text: "SUMMARY"}
		var output bytes.Buffer
		h := NewHandler().WithBranchSummaries(summaries).WithSegmentCollector(func(dir string) { woken = append(woken, dir) })
		for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
			if err := run(t, h, domain.ProviderCodex, event, hookPayload{Cwd: cwd, SessionID: "44444444-4444-4444-8444-444444444444"}, &output); err != nil {
				t.Fatalf("%s: %s: %v", name, event, err)
			}
		}
		if len(woken) != 0 || len(summaries.inputs) != 0 || output.Len() != 0 {
			t.Fatalf("%s: woke %v, asked %d times, wrote %q", name, woken, len(summaries.inputs), output.String())
		}
	}
	if _, err := os.Stat(filepath.Join(notSetUp, ".cxt")); !os.IsNotExist(err) {
		t.Fatalf("a hook created .cxt in a repository that was not set up: %v", err)
	}
	// A .cxt directory left behind is kept out of Git, and is still not a mark.
	exclude, err := os.ReadFile(filepath.Join(residue, ".git", "info", "exclude"))
	if err != nil || !strings.Contains(string(exclude), ".cxt/") {
		t.Fatalf("a leftover .cxt directory is not ignored by Git: %v %q", err, exclude)
	}
}

// What a tool passes that is not the expected JSON is not an error.
func TestHandlerReadsAnUnexpectedPayloadAsEmpty(t *testing.T) {
	cwd := gitRepository(t, true)
	t.Chdir(cwd)
	var woken []string
	h := NewHandler().WithSegmentCollector(func(dir string) { woken = append(woken, dir) })
	h.stdin, h.stdout = strings.NewReader("{not json"), &bytes.Buffer{}
	if err := h.Run(domain.ProviderClaude, "Stop"); err != nil {
		t.Fatal(err)
	}
	// Without a folder in the payload the hook's own folder is the session's.
	if len(woken) != 1 {
		t.Fatalf("woke %v", woken)
	}
}
