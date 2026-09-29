// agent_wrapper.go — Agent CLI Wrapper (default execution path): cxt claude / cxt codex.
//
// Spawns the agent as a child process (stdio inheritance — same usage) and monitors .cxt/boundary.json.
// Automatically restarts the child as a seed session when context switch (branch move/creation) is detected — "change branch, agent restarts itself in new context".
// Fresh interactive launches prepare a verified package; native resume is not reinjected.
// No dependency on provider API; uses process ownership (POSIX) only.
package cli

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/boundary"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// runAgentWrapper is retained for existing callers. Composition supplies hooks
// for a fresh managed launch; native resume and diagnostic modes need none.
func runAgentWrapper(ctx context.Context, _ *Container, cwd, agent string, args []string, preparedHooks ...ProviderLaunchHooks) error {
	var hooks ProviderLaunchHooks
	if len(preparedHooks) > 0 {
		hooks = preparedHooks[0]
	}
	return RunProviderLaunch(ctx, cwd, LaunchIntent{Provider: domain.ProviderKind(agent), ProviderArgs: args}, hooks)
}

// newBoundarySince checks if a transition boundary has been recorded since start.
func newBoundarySince(cwd string, start time.Time) bool {
	b, ok := boundary.Load(cwd)
	if !ok {
		return false
	}
	at, err := time.Parse(time.RFC3339, b.At)
	return err == nil && at.After(start)
}

func boundaryLoad(cwd string) (boundary.Boundary, bool) { return boundary.Load(cwd) }

func resumeArgs(agent, seedID string) []string {
	if agent == "codex" {
		return []string{"resume", seedID}
	}
	return []string{"--resume", seedID}
}

// sessionIDFromAgentArgs projects only an explicit native resume target into
// the child environment. Descendant cxt commands can then bind capture to this
// wrapper's exact session instead of the newest sibling terminal. Empty is
// intentional for a provider-created fresh session; its first lifecycle hook
// records terminal affinity.
func sessionIDFromAgentArgs(agent string, args []string) string {
	if agent != string(domain.ProviderCodex) && agent != string(domain.ProviderClaude) {
		return ""
	}
	inv, err := inspectProviderInvocation(domain.ProviderKind(agent), args)
	if err != nil {
		return ""
	}
	return inv.SessionID
}

// Registers the recently materialized session in the agent's session index.
// Codex TUI's resume-by-id only queries the threads DB (empirically verified, 0.143 —
// unlike non-tty paths, it lacks file scan fallback, resulting in "No saved session found").
// 1st step: Directly register in threads DB (instant, deterministic, sqlite3 CLI — matches the actual codex backfill row).
// 2nd step: Backfill warming via `codex resume` (without model call, immediate termination — backfill is async, this alone does not launch and race).
// Fallback-open — even on failure, codex backfill registers eventually.
// Claude uses file-based lookup, making this step unnecessary.
func warmAgentIndexContext(ctx context.Context, bin, agent, cwd, seedID string) {
	if agent != "codex" {
		return
	}
	registerCodexThread(cwd, seedID)
	warmCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	warm := exec.CommandContext(warmCtx, bin, "resume")
	warm.Dir = cwd
	warm.Env = providerLaunchEnvironment(warmCtx, cwd, agent, []string{"resume"}, false)
	_ = warm.Run() // no stdin/stdout attached (non-TTY); exits immediately with "stdin is not a terminal"
}

// Upserts the materialized rollout directly into the codex threads DB.
// Schema is versioned (state_<N>.sqlite) — selects the latest file, silently ignores INSERT failures (backfill fallback).
func registerCodexThread(cwd, seedID string) {
	if !providerfs.ValidSessionID(seedID) {
		return
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		return
	}
	root, err := providerfs.CodexSessionsDir()
	if err != nil {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*", "*", "*", "rollout-*-"+seedID+".jsonl"))
	if len(matches) == 0 {
		return
	}
	rollout, err := providerfs.OpenRegularFile(matches[0])
	if err != nil {
		return
	}
	_ = rollout.Close()
	dbs, _ := filepath.Glob(filepath.Join(filepath.Dir(root), "state_*.sqlite"))
	if len(dbs) == 0 {
		return
	}
	sort.Strings(dbs)
	db := dbs[len(dbs)-1]
	dbFile, err := providerfs.OpenRegularFile(db)
	if err != nil {
		return
	}
	_ = dbFile.Close()
	esc := func(s string) string { return strings.ReplaceAll(s, "'", "''") }
	sql := fmt.Sprintf(
		`INSERT OR IGNORE INTO threads (id, rollout_path, created_at, updated_at, source, model_provider, cwd, title, sandbox_policy, approval_mode) `+
			`VALUES ('%s','%s',strftime('%%s','now'),strftime('%%s','now'),'cli','openai','%s','','{"type":"read-only"}','on-request');`,
		esc(seedID), esc(matches[0]), esc(cwd))
	_ = exec.Command(sqlite, db, sql).Run()
}

// activeSessionID resolves the same capture-eligible provider file used by hook
// capture and decodes its native identity. Filename/mtime guesses are not
// sufficient: multiple terminals can run the same provider in one worktree.
// Failure is conservative — Load then uses its original reachability rule.
func activeSessionID(ctx context.Context, cwd, agent string) string {
	provider := domain.ProviderKind(agent)
	if affinity := capture.SessionAffinity(cwd, provider); affinity != "" {
		return affinity
	}
	var src interface {
		LocateActiveSession(context.Context, string) (string, error)
		ReadSession(context.Context, string) ([]byte, error)
	}
	var cdc interface {
		Decode(context.Context, []byte) (domain.CIRDocument, error)
	}
	if agent == string(domain.ProviderCodex) {
		src = capture.NewCodexCapture()
		cdc = codec.NewCodexCodec()
	} else if agent == string(domain.ProviderClaude) {
		src = capture.NewClaudeCapture()
		cdc = codec.NewClaudeCodec()
	} else {
		return ""
	}
	path, err := src.LocateActiveSession(ctx, cwd)
	if err != nil {
		return ""
	}
	raw, err := src.ReadSession(ctx, path)
	if err != nil {
		return ""
	}
	cir, err := cdc.Decode(ctx, raw)
	if err != nil {
		return ""
	}
	return cir.Envelope.SessionOriginID
}
