// Package hook contains the hook event handlers for the cxt hook subcommand (domain model, capture path).
//
// Usage: cxt hook --provider <claude|codex> --event <Name>
//
// Event-specific actions (capture path):
//
//	SessionStart(claude/codex):   CaptureCoordinator.MarkBaseline (no commit)
//	Stop(claude/codex):           CaptureCoordinator.RequestCapture(debounce=true)
//	SessionEnd(claude/codex):     CaptureCoordinator.RequestCapture(force=true) — force flush
//	UserPromptSubmit(claude/codex): CaptureCoordinator.MarkTurn (no commit)
//
// Safety Contract (invariants, capture path):
//   - Always exit 0 (errors reported only to stderr — cmd/cxt hook branch ensures)
//   - Terminate within 10 seconds (hooks.json timeout:10) — double defense with 8-second context timeout
//   - Read original session file as read-only
//   - ErrNoActiveSession → no-op silently
package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/githooks"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Handler is the hook event handler. Keep it thin (capture path) —
// parse stdin and classify events, delegate core logic to CaptureCoordinator.
type Handler struct {
	coord   *capture.CaptureCoordinator
	observe func(string, domain.ProviderKind, string)
	stdin   io.Reader // for testing. nil means os.Stdin (no read from terminal)
	stdout  io.Writer // for testing. nil means os.Stdout (additionalContext JSON emission channel)
}

// NewHandler creates a Handler and injects CaptureCoordinator.
func NewHandler(coord *capture.CaptureCoordinator) *Handler {
	return &Handler{coord: coord}
}

// hookPayload contains the known keys from provider hook stdin JSON.
type hookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"` // claude
	RolloutPath    string `json:"rollout_path"`    // Codex hint; falls back to discovery when absent
	Cwd            string `json:"cwd"`
	Prompt         string `json:"prompt"` // UserPromptSubmit
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
	path := p.TranscriptPath
	if path == "" {
		path = p.RolloutPath
	}

	// Codex hooks are registered globally, so they run in every repository.
	// Repair ignore rules for any existing store (including legacy residue),
	// then require cxt init/setup's durable HEAD marker before writing capture
	// state. A bare .cxt directory is not user opt-in.
	switch event {
	case "SessionStart", "UserPromptSubmit", "Stop", "SessionEnd":
	default:
		return nil
	}
	state := gitctx.InspectContextRoot(ctx, cwd)
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.Initialized {
		// Payload IDs may be inherited by provider child processes. Never infer
		// a corrected ID from their path or let a mismatch touch parent state.
		if err := capture.ValidateHookSession(cwd, provider, p.SessionID, path); err != nil {
			return err
		}
	}
	if state.GitRepository && state.Exists {
		_ = githooks.EnsureIgnored(state.Root)
	}
	if !state.GitRepository || !state.Initialized {
		return nil
	}
	if path == "" && p.SessionID != "" {
		// Correct discovery can replace an older poisoned pointer. This stays
		// within the hook's exact worktree and never uses command-only fallback.
		if resolved, err := capture.LocateCaptureSession(ctx, provider, cwd, p.SessionID); err == nil {
			path = resolved
		} else if event == "SessionEnd" {
			return err
		}
	}
	if err := capture.TrackAppSession(cwd, provider, p.SessionID, path); err != nil {
		return err
	}

	switch event {
	case "SessionStart":
		err := h.coord.MarkBaseline(ctx, provider, cwd, path, p.SessionID)
		h.emitBriefing(event, cwd, p.SessionID) // app branch handoff + pull briefing
		if h.observe != nil {
			h.observe(cwd, provider, p.SessionID)
		}
		return err
	case "UserPromptSubmit":
		err := h.coord.MarkTurn(ctx, provider, cwd, p.SessionID, p.Prompt)
		h.emitBriefing(event, cwd, p.SessionID) // app branch handoff + team context notice
		if h.observe != nil {
			h.observe(cwd, provider, p.SessionID)
		}
		return err
	case "Stop":
		captured, err := h.coord.RequestCapture(ctx, provider, cwd, path, p.SessionID, true, false)
		if captured {
			spawnPendingSync(cwd)
		}
		return err
	case "SessionEnd":
		// Resolve and persist the exact transcript before waiting on another
		// capture. If the deadline or save fails, later hooks/checkpoints can
		// retry using the retained on-disk liveness pointer.
		captured, err := h.coord.RequestCapture(ctx, provider, cwd, path, p.SessionID, false, true)
		if err == nil {
			capture.EndAppSession(cwd, provider, p.SessionID)
		}
		if captured {
			spawnPendingSync(cwd)
		}
		return err
	}
	return nil
}

// emitBriefing consumes the current terminal's identifier-only team context
// notice (generated by git pull) and emits it as JSON to stdout via
// hookSpecificOutput.additionalContext.
// This output is not visible to users but is passed only to the model. Empirically verified (0.143):
// The codex binary contains additionalContext/HookOutputEntry/EventName (sessionStart, userPromptSubmit, etc.) — confirmed as compatible with the Claude Code hook schema.
func (h *Handler) emitBriefing(event, cwd, sessionID string) {
	var entries []string
	if text, ok := capture.ConsumeSessionHandoff(cwd, sessionID); ok {
		entries = append(entries, text)
	}
	if text, ok := capture.ConsumeBriefing(cwd); ok {
		entries = append(entries, text)
	}
	if len(entries) == 0 {
		return
	}
	text := strings.Join(entries, "\n\n")
	out := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName":     event,
			"additionalContext": text,
		},
	}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	w := h.stdout
	if w == nil {
		w = os.Stdout
	}
	_, _ = fmt.Fprintln(w, string(b))
}

// spawnPendingSync spawns a detached helper to reflect uncommitted capture pointers to the server.
// The hook process is latency-sensitive and does not directly use the network.
// A separate process performs best-effort work, and later capture or push
// reconciliation recovers failures.
func spawnPendingSync(cwd string) {
	repoRoot, enabled := gitctx.ContextRoot(context.Background(), cwd)
	if !enabled {
		return
	}
	if _, ok := remotecfg.Origin(repoRoot); !ok && os.Getenv("CXT_REMOTE") == "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "git-hook", "pending-sync")
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Start()
}

// An observer is started only for the hook's proven exact-worktree identity.
// It never holds up the hook and exits when the registry is removed or idle.
func spawnLiveObserver(cwd string, provider domain.ProviderKind, id string) {
	if _, ok := capture.RegisteredSession(cwd, provider, id); !ok {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "git-hook", "live-watch", string(provider), id)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// WithLiveObservation enables process creation in the production composition
// root. Tests can inject a recorder without ever launching detached helpers.
func (h *Handler) WithLiveObservation() *Handler { h.observe = spawnLiveObserver; return h }
