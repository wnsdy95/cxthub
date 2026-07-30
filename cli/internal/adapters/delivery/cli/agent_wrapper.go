// agent_wrapper.go — Agent CLI Wrapper (default execution path): cxt claude / cxt codex.
//
// Spawns the agent as a child process (stdio inheritance — same usage) and monitors .cxt/boundary.json.
// Automatically restarts the child as a seed session when context switch (branch move/creation) is detected — "change branch, agent restarts itself in new context".
// An argumentless fresh start starts with the current branch HEAD context as the seed ("on start, inject").
// No dependency on provider API; uses process ownership (POSIX) only.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/boundary"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// runAgentWrapper supervises the execution of agent (claude|codex).
func runAgentWrapper(ctx context.Context, c *Container, cwd, agent string, args []string) error {
	bin, err := exec.LookPath(agent)
	if err != nil {
		return fmt.Errorf("%s executable file not found (PATH check): %w", agent, err)
	}
	// Flags-only execution (`cxt codex --yolo` etc.) is also a fresh start — attaches flags to the seed.
	// Otherwise, a single flag would disable context injection entirely (false positive).
	// If the first argument is not a dash (subcommand `codex exec`/prompt), the execution form is as specified by the user.
	// Claude session selection flags (--resume/--continue) conflict with the seed, so they are also passed (user intent takes precedence).
	var passFlags []string
	if len(args) > 0 && strings.HasPrefix(args[0], "-") && !hasSessionFlag(agent, args) {
		passFlags = args
		args = nil
	}
	// Argumentless fresh start → resumes the current branch context (wrapper = context-managed execution; start with empty session if agent binary is called directly). Failure is fail-open.
	if len(args) == 0 {
		// Non-interactive stdin (pipe/redirection): seed resume requires an interactive TTY, and the agent
		// empirically exits with "Provide a prompt". Explain the cause before launching it.
		if fi, serr := os.Stdin.Stat(); serr == nil && fi.Mode()&os.ModeCharDevice == 0 {
			fmt.Fprintf(os.Stderr, "cxt: non-interactive stdin — context seed resume works in interactive terminal. Non-interactive execution requires direct call to %s (e.g., %s -p \"...\")\n", agent, agent)
		}
		seed := seedFromBranch(ctx, c, cwd, agent)
		if len(seed) > 0 {
			warmAgentIndex(bin, agent, cwd, seed[len(seed)-1])
		}
		args = append(seed, passFlags...)
	}
	for {
		start := time.Now()
		child := exec.Command(bin, args...)
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
		child.Env = append(os.Environ(),
			"CXT_WRAPPED=1",
			fmt.Sprintf("CXT_WRAPPER_PID=%d", os.Getpid()),
			"CXT_WRAPPED_AGENT="+agent,
		)
		if err := child.Start(); err != nil {
			return err
		}
		done := make(chan error, 1)
		go func() { done <- child.Wait() }()

		var exitErr error
		terminated := false
	watch:
		for {
			select {
			case exitErr = <-done:
				break watch
			case <-time.After(1 * time.Second):
				if newBoundarySince(cwd, start) {
					// Context switch detected → child termination (kill and contention are harmless — same end result).
					_ = child.Process.Signal(syscall.SIGTERM)
					exitErr = <-done
					terminated = true
					break watch
				}
			}
		}
		// Child finished (by itself, by execution kill, or by our kill) — Restart with new boundary as seed if present.
		if !terminated && !newBoundarySince(cwd, start) {
			return exitErr // Normal exit without transition
		}
		b, ok := boundaryLoad(cwd)
		if !ok || !providerfs.ValidSessionID(b.SeedID) {
			return exitErr // Boundary exists but no resume target (e.g., detached) — Exit
		}
		fmt.Printf("\ncxt: ⚠ Context transition (%s → %s) — Restarting with seed session…\n", b.PrevBranch, b.Branch)
		args = append(resumeArgs(agent, b.SeedID), passFlags...) // Preserve user flags (e.g., --yolo) for restart
		warmAgentIndex(bin, agent, cwd, b.SeedID)                // Transition seed also needs recent materialized file — Index registration required
		time.Sleep(300 * time.Millisecond)                       // Terminal cleanup buffer
	}
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

// Determines if a session selection flag conflicts with resuming.
// Claude conflicts with --resume/--continue flags, causing --resume to be redundant.
// Codex handles session selection as a subcommand (resume), avoiding flag conflicts.
func hasSessionFlag(agent string, args []string) bool {
	if agent != "claude" {
		return false
	}
	for _, a := range args {
		switch a {
		case "--resume", "-r", "--continue", "-c":
			return true
		}
	}
	return false
}

func resumeArgs(agent, seedID string) []string {
	if agent == "codex" {
		return []string{"resume", seedID} // May not be supported in all versions — falls back to new session on failure
	}
	return []string{"--resume", seedID}
}

// Registers the recently materialized session in the agent's session index.
// Codex TUI's resume-by-id only queries the threads DB (empirically verified, 0.143 —
// unlike non-tty paths, it lacks file scan fallback, resulting in "No saved session found").
// 1st step: Directly register in threads DB (instant, deterministic, sqlite3 CLI — matches the actual codex backfill row).
// 2nd step: Backfill warming via `codex resume` (without model call, immediate termination — backfill is async, this alone does not launch and race).
// Fallback-open — even on failure, codex backfill registers eventually.
// Claude uses file-based lookup, making this step unnecessary.
func warmAgentIndex(bin, agent, cwd, seedID string) {
	if agent != "codex" {
		return
	}
	registerCodexThread(cwd, seedID)
	warm := exec.Command(bin, "resume")
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

// Materializes the current branch HEAD context into an agent session and returns the resume arguments — "cxt <agent> starts with that branch's context". If no branch/context or materialization fails, returns nil (pure execution). If full materialization is not possible, Load is forced into memory mode, injecting into the provider memory file (CLAUDE.md/AGENTS.md), making it a valid injection.
func seedFromBranch(ctx context.Context, c *Container, cwd, agent string) []string {
	branch := gitOut(cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" || branch == "HEAD" {
		return nil
	}
	// PreferPendingTail: If the last session (pre-commit hook capture) connects to the head, use it as the seed — codex transitions from cxt claude without a commit.
	out, err := c.Load.Load(ctx, inbound.LoadInput{Ref: branch, Cwd: cwd, TargetProvider: domain.ProviderKind(agent), PreferPendingTail: true})
	if err != nil {
		return nil // No context in branch (new repo, etc.) — pure execution
	}
	if out.ResumeCmd == "" {
		if out.WrittenPath != "" {
			fmt.Printf("cxt: %q branch context injected into memory format (%s) — starting new session\n", branch, out.WrittenPath)
		}
		return nil
	}
	// ResumeCmd format: "claude --resume <id>" / "codex resume <id>" — takes only the execution arguments.
	f := strings.Fields(out.ResumeCmd)
	if len(f) < 2 || f[0] != agent {
		return nil
	}
	note := ""
	if out.TrimmedEvents > 0 {
		note = fmt.Sprintf(" — seeds recent history with context window budget (omits %d old events)", out.TrimmedEvents)
	}
	fmt.Printf("cxt: %q branch context started as seed (fidelity: %s)%s\n", branch, out.Fidelity, note)
	return f[1:]
}
