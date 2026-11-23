// Package hook contains the handler of `cxt hook`, which the agent tools run
// at the events of a session.
//
// Usage: cxt hook --provider <claude|codex> --event <Name>
//
// What an event does:
//
//	SessionStart, UserPromptSubmit: give the session the most recent summary
//	    along the branch its folder is on, once for each branch it comes to
//	every handled event: wake the collector that reads the tools' own session
//	    records and Git
//
// A hook carries no data to the server and decides nothing: what a session
// said and did is read from the tool's records, whether or not a hook ran.
//
// Safety contract:
//   - Always exit 0 (errors reported only to stderr — cmd/cxt hook branch ensures)
//   - Terminate within 10 seconds (hooks.json timeout:10) — double defense with 8-second context timeout
//   - Never write to the tool's own files
package hook

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/githooks"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

// Handler is the hook event handler.
type Handler struct {
	// summaries gives a session the most recent summary along its branch.
	summaries inbound.BranchSummaries
	// wakeCollector starts the detached session report collector. An agent
	// hook only wakes it; the collector reads what the tools recorded.
	wakeCollector func(string)
	stdin         io.Reader // for testing. nil means os.Stdin (no read from terminal)
	stdout        io.Writer // for testing. nil means os.Stdout (additionalContext JSON emission channel)
}

// NewHandler creates a Handler.
func NewHandler() *Handler {
	return &Handler{}
}

// WithSegmentCollector makes every handled agent hook wake the session report
// collector after its own work.
func (h *Handler) WithSegmentCollector(wake func(string)) *Handler {
	h.wakeCollector = wake
	return h
}

// hookPayload contains the known keys from provider hook stdin JSON.
type hookPayload struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
}

// readPayload attempts best-effort parsing of stdin JSON. If not from a terminal (not a pipe),
// it reads only known keys even if the schema differs — failure results in an empty payload (fallback), not an error.
func (h *Handler) readPayload() hookPayload {
	var p hookPayload
	r := h.stdin
	if r == nil {
		fi, err := os.Stdin.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
			return p
		}
		r = os.Stdin
	}
	b, err := io.ReadAll(io.LimitReader(r, 256<<10))
	if err != nil {
		return p
	}
	_ = json.Unmarshal(b, &p)
	return p
}

// Run handles hook events. Unknown events are no-op (fail-open).
// Return errors are reported only to stderr by the caller (cmd/cxt) and exit 0 is maintained.
func (h *Handler) Run(provider domain.ProviderKind, event string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return h.run(ctx, provider, event)
}

func (h *Handler) run(ctx context.Context, provider domain.ProviderKind, event string) error {
	p := h.readPayload()
	cwd := p.Cwd
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	switch event {
	case "SessionStart", "UserPromptSubmit", "Stop", "SessionEnd":
	default:
		return nil
	}
	// Codex hooks are registered globally, so they run in every repository.
	// Only a folder where `cxt setup` or `cxt init` ran is handled: both write
	// .cxt/HEAD, and a bare .cxt directory is not the user's choice.
	state := gitctx.InspectContextRoot(ctx, cwd)
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.GitRepository && state.Exists {
		_ = githooks.EnsureIgnored(state.Root)
	}
	if !state.GitRepository || !state.Initialized {
		return nil
	}
	if h.wakeCollector != nil {
		defer h.wakeCollector(cwd)
	}
	if event != "SessionStart" && event != "UserPromptSubmit" {
		return nil
	}
	return h.giveSummary(ctx, provider, event, cwd, p.SessionID)
}

// WithBranchSummaries makes a session's hooks give it the most recent summary
// along the branch its folder is on, once for each branch it comes to. It
// takes the place of the queued handoff: the summary is read from the server
// and from Git as they are when the hook runs, whatever ran at the checkout.
func (h *Handler) WithBranchSummaries(summaries inbound.BranchSummaries) *Handler {
	h.summaries = summaries
	return h
}

// branchSummaryWait bounds how long a prompt waits for the server's answer.
const branchSummaryWait = 4 * time.Second

// giveSummary uses the documented SessionStart/UserPromptSubmit JSON channel:
// https://learn.chatgpt.com/docs/hooks and https://code.claude.com/docs/en/hooks.
// The summary counts as given only after the complete JSON write.
func (h *Handler) giveSummary(ctx context.Context, provider domain.ProviderKind, event, cwd, sessionID string) error {
	if h.summaries == nil {
		return nil
	}
	read, cancel := context.WithTimeout(ctx, branchSummaryWait)
	defer cancel()
	return h.summaries.DeliverBranchSummary(read, inbound.BranchSummaryInput{Cwd: cwd, Provider: provider, SessionID: sessionID}, func(text string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return h.writeContext(event, []string{text})
	})
}

func (h *Handler) writeContext(event string, entries []string) error {
	if len(entries) == 0 {
		return nil
	}
	out := map[string]interface{}{"hookSpecificOutput": map[string]interface{}{"hookEventName": event, "additionalContext": strings.Join(entries, "\n\n")}}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w := h.stdout
	if w == nil {
		w = os.Stdout
	}
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}
