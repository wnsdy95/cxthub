// githook.go — event handlers called by git hooks (scripts targeted by githooks adapters).
//
// "Using git automatically follows cxt":
//
//	git commit          → post-commit   → active session snapshot (commit message + SHA link)
//	git checkout -b X   → post-checkout → fork current context to X
//	git checkout X      → post-checkout → restore context X (auto) or prompt (prepare)
//	git push            → pre-push      → cxt push (when origin is registered)
//	git pull/merge      → post-merge    → cxt pull (when origin is registered)
//
// All handlers are fail-open: failures are logged as stderr warnings and return nil,
// never blocking git operations.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/boundary"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// gitOut runs a git subcommand in cwd and returns the trimmed stdout (empty on failure).
func gitOut(cwd string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// hookWarn notifies of misconfiguration failures in hooks to stderr (git continues).
func hookWarn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "cxt: "+format+"\n", a...)
}

// syncWarn notifies of push/pull failures in hooks. Authentication issues (401/403) are warned once and then suppressed — local snapshots continue to accumulate, so the next push will pick up any missed changes after login.
func syncWarn(cwd, op string, err error) {
	msg := err.Error()
	is401 := strings.Contains(msg, "401")
	is403 := strings.Contains(msg, "403")
	if !is401 && !is403 {
		hookWarn("context %s failed (git continues): %v", op, err)
		return
	}
	if _, serr := providerfs.ReadRepoFile(cwd, filepath.Join(".cxt", "auth-hint-shown")); serr == nil {
		return // already notified — suppress
	}
	_ = providerfs.WriteRepoFileAtomic(cwd, filepath.Join(".cxt", "auth-hint-shown"), []byte("1"), 0o644)
	if is401 {
		hookWarn("context %s delayed — login required: generate token in Web Account Settings ⚙ and run cxt login <token>. Local records continue to accumulate and will be automatically picked up after login (this notice is shown once)", op)
	} else {
		hookWarn("context %s delayed — insufficient permissions (read-only role or policy restrictions). Request role promotion from owner (this notice is shown once)", op)
	}
}

// clearAuthHint deletes the marker on sync success — subsequent auth issues will be re-introduced once.
func clearAuthHint(cwd string) {
	_ = providerfs.RemoveRepoFile(cwd, filepath.Join(".cxt", "auth-hint-shown"))
}

// commitProviders chooses the providers to snapshot for this commit. If cxt add staged providers,
// it uses that set; otherwise it tries every provider that may have an active session (Claude and Codex).
// Defaulting to Claude alone would let Codex commits pass without context, make post-commit capture stale
// Claude residue, and omit Codex from transition checkpoints. Trying an inactive provider is harmless because
// Save quietly skips ErrNoActiveSession.
func commitProviders(cwd string) []string {
	if staged := remotecfg.StagedProviders(cwd); len(staged) > 0 {
		return staged
	}
	return []string{domain.ProviderClaude, domain.ProviderCodex}
}

// snapshotForCommit snapshots active sessions by provider (common for commits/hooks).
// Returns: number of successful snapshots.
func snapshotForCommit(ctx context.Context, c *Container, cwd, message string) (int, error) {
	saved := 0
	var lastErr error
	var resolved []string
	for _, p := range commitProviders(cwd) {
		out, err := c.Save.Save(ctx, inbound.SaveInput{Cwd: cwd, Provider: p, Message: message, Author: c.Identity})
		if err != nil {
			// No active session is normal (unused agent commit) — skip silently.
			if err == domain.ErrNoActiveSession {
				continue
			}
			lastErr = err
			hookWarn("%s session snapshot failed: %v", p, err)
			continue
		}
		fmt.Printf("cxt: snapshot %s (%s) on %q\n", shortHash(out.SnapshotID), p, out.Branch)
		saved++
		if out.SessionID != "" {
			resolved = append(resolved, out.SessionID)
		}
		// Auto memorize: attaches the digest to the just saved head snapshot — per provider
		// each has its own (native sources differ: claude=MEMORY.md, codex=memories sqlite). Mixed commits
		// also have each snapshot carry its own digest, and cross-inherit memory through lineage merge (nearestAncestorDigest).
		// The next push carries it with the raw data. Best-effort (the commit remains valid on failure).
		if mout, merr := c.Memorize.Memorize(ctx, inbound.MemorizeInput{Cwd: cwd, Provider: p, Ref: string(out.SnapshotID)}); merr == nil {
			fmt.Printf("cxt: memorized (%s) → %s (included in next push)\n", p, shortHash(mout.MemoryHash))
		}
	}
	if saved > 0 {
		_ = remotecfg.SetStagedProviders(cwd, nil) // exhaust staged providers
		// Absorb the remote pending of the session that was committed + reflect remaining pending (detached — no commit delay).
		spawnPendingSync(cwd, resolved)
	}
	return saved, lastErr
}

// spawnPendingSync spawns the pending-sync detached helper (best-effort, no hook delay).
func spawnPendingSync(cwd string, resolveSessions []string) {
	if _, ok := remotecfg.Origin(cwd); !ok && os.Getenv("CXT_REMOTE") == "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	hookArgs := []string{"git-hook", "pending-sync"}
	for _, sid := range resolveSessions {
		hookArgs = append(hookArgs, "--resolve", sid)
	}
	cmd := exec.Command(exe, hookArgs...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Start()
}

// acquireSyncLock acquires the pending-sync serialization lock (.cxt/sync.lock).
// Multiple detached helpers spawned on commit/agent stop reduce contention on SyncPendings to recover stale unsyncs (backlog #2 — self-heal with server and flicker removal).
// Briefly wait and retry to ensure --resolve deletion is not lost, and proceed without lock on deadline exceeded (availability priority — server self-heals by reachability, so safe). O_EXCL + stale argument pattern.
func acquireSyncLock(cwd string) (release func()) {
	lock, err := providerfs.PrepareRepoFile(cwd, filepath.Join(".cxt", "sync.lock"), 0o755)
	if err != nil {
		return func() {}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lock) }
		}
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > 2*time.Minute {
			_ = os.Remove(lock) // Remove crash residual
			continue
		}
		if time.Now().After(deadline) {
			return func() {} // Proceed without lock
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// contextSwitch is the context sequence for branch switch (post-checkout flag=1):
//
// Checkpoint (update and push to previous branch) → Isolation (exclude permanent capture of old session) →
// Recovery/Seed (load existing branch = head, new branch = seed birth) → Boundary signal (+execute)
//
// Alignment with git intent: switch is "continue working" (load only target context), -b is
// "start new work" (seed birth reason — cut), detached is "time travel" (no capture).
// Opt-out: CXT_KEEP_SESSION=1 (suppress switch — keep old session), CXT_CARRY=1 (carry session).
func contextSwitch(ctx context.Context, c *Container, cwd string) error {
	if os.Getenv("CXT_KEEP_SESSION") == "1" {
		hookWarn("keep-session — context switch omitted (current session maintained)")
		return nil
	}
	branch := gitOut(cwd, "rev-parse", "--abbrev-ref", "HEAD")
	prevBranch := gitOut(cwd, "rev-parse", "--abbrev-ref", "@{-1}")
	detached := branch == "" || branch == "HEAD"
	targetProvider, wrapperManaged := supervisedProvider()

	// 1) Checkpoint: snapshot and push (best-effort) the live sessions from the previous branch.
	//    Ensure isolation target file paths here (direct use of capture adapter — hook-specific paths).
	type liveSession struct {
		provider string
		path     string
	}
	var lives []liveSession
	for _, p := range commitProviders(cwd) {
		var src interface {
			LocateActiveSession(context.Context, string) (string, error)
		}
		if p == domain.ProviderCodex {
			src = capture.NewCodexCapture()
		} else {
			src = capture.NewClaudeCapture()
		}
		if path, err := src.LocateActiveSession(ctx, cwd); err == nil {
			lives = append(lives, liveSession{provider: p, path: path})
		}
	}
	checkpointed := 0
	for _, ls := range lives {
		msg := "checkpoint: switch"
		if prevBranch != "" && !detached {
			msg = fmt.Sprintf("checkpoint: %s → %s", prevBranch, branch)
		}
		if out, err := c.Save.Save(ctx, inbound.SaveInput{Cwd: cwd, Provider: ls.provider, Message: msg, Author: c.Identity, Branch: prevBranch}); err == nil {
			fmt.Printf("cxt: checkpoint %s (%s) on %q\n", shortHash(out.SnapshotID), ls.provider, out.Branch)
			checkpointed++
			// Memorize checkpoints as well, falling back to an ancestor digest when the tip has none (audit finding #5). HEAD is already the new branch, so identify it explicitly by ref.
			_, _ = c.Memorize.Memorize(ctx, inbound.MemorizeInput{Cwd: cwd, Provider: ls.provider, Ref: string(out.SnapshotID)})
		}
	}
	if checkpointed > 0 {
		if _, ok := remotecfg.Origin(cwd); ok {
			if _, err := c.Sync.Push(ctx, inbound.SyncInput{Cwd: cwd}); err != nil {
				// Irrelevant sequels etc. non-ff are appended with retry (latest update before switch must be lossless).
				_, _ = c.Sync.Push(ctx, inbound.SyncInput{Cwd: cwd, Append: true})
			}
		}
	}

	if os.Getenv("CXT_CARRY") == "1" {
		hookWarn("carry — current session carried over to %q (no isolation/recovery)", branch)
		return nil
	}
	if remotecfg.CheckoutMode(cwd) == remotecfg.CheckoutPrepare {
		// Prepare: only up to checkpoint — isolation/recovery must be explicitly done by user (cxt checkout).
		fmt.Printf("cxt: prepare mode — no recovery. Context switch: cxt checkout %s\n", branch)
		return nil
	}

	// 2) Isolation: old session ended at switch point — subsequent utterances are not recorded.
	// Isolates all session files in this cwd, not just the latest session: branch switch changes disk state (all session prerequisites), so must prevent remaining sessions from being erroneously active in next commit (multi-terminal cases included). Applies to claude/codex as well.
	var superseded []string
	for _, p := range append(claudeSessionFiles(cwd), codexSessionFiles(ctx, cwd)...) {
		if renamed := boundary.Supersede(cwd, p); renamed != "" {
			superseded = append(superseded, renamed)
		}
	}

	// 3) Recovery/seed.
	b := boundary.Boundary{PrevBranch: prevBranch, Branch: branch, Superseded: superseded}
	switch {
	case detached:
		fmt.Println("cxt: detached HEAD — time travel mode (no context capture). Recovery: cxt checkout <ref>")
	default:
		existing, lerr := c.List.List(ctx, inbound.ListInput{Branch: branch})
		// fork-only ref (web fork connection·git branch fork) exists without label snapshot —
		// this is also an "existing branch" (seed override invalidates fork connection).
		hasRef := false
		if lerr == nil {
			for _, r := range existing.Refs {
				if r.Kind == domain.RefBranch && r.Name == branch && r.Target != "" {
					hasRef = true
					break
				}
			}
		}
		if lerr == nil && (len(existing.Snapshots) > 0 || hasRef) {
			// Existing branch: load context of that task (current context is not carried).
			if out, err := c.Checkout.Checkout(ctx, inbound.CheckoutInput{From: branch, TargetProvider: targetProvider, Mode: hookLoadMode(cwd), Cwd: cwd}); err == nil {
				b.SeedPath, b.ResumeCmd = out.WrittenPath, out.ResumeCmd
				b.SeedID = restoredSessionID(out.WrittenPath, out.ResumeCmd, "")
				fmt.Printf("cxt: %q context prepared  [fidelity: %s]\n", branch, out.Fidelity)
				syncSettingsToSnapshot(ctx, c, cwd, out.Head, "git checkout "+branch)
			}
		} else if prevBranch != "" && prevBranch != branch {
			// New branch: seed genesis — main memory ⊕ ancestry summary + previous commit raw tail.
			if out, err := c.Seed.Seed(ctx, inbound.SeedInput{Cwd: cwd, FromBranch: prevBranch, NewBranch: branch, Provider: targetProvider, Author: c.Identity}); err == nil {
				b.SeedPath, b.ResumeCmd = out.WrittenPath, out.ResumeCmd
				b.SeedID = restoredSessionID(out.WrittenPath, out.ResumeCmd, out.SessionID)
				fmt.Printf("cxt: seed created → branch %q (snapshot %s)\n", branch, shortHash(out.SnapshotID))
				if _, ok := remotecfg.Origin(cwd); ok {
					if _, perr := c.Sync.Push(ctx, inbound.SyncInput{Cwd: cwd}); perr != nil && strings.Contains(perr.Error(), domain.ErrSyncConflict.Error()) {
						// Remote branch with same name (web fork etc.) already exists — seed principle (transient creation is always seed) enforces local retention, push reordering is warned.
						hookWarn("remote context branch %q exists with same name (web fork?) — push reordering will occur", branch)
					}
				}
			}
		}
	}

	// 4) Boundary signal + execution.
	if len(superseded) > 0 || b.SeedPath != "" {
		if !boundaryTransitionSafe(b, wrapperManaged) {
			hookWarn("context boundary has no valid resume target; active agent was not terminated — continue explicitly with cxt checkout %s", branch)
			return nil
		}
		if err := boundary.Record(cwd, b); err != nil {
			hookWarn("context boundary was not recorded; active agent was not terminated: %v", err)
			return nil
		}
		if b.ResumeCmd != "" {
			fmt.Printf("cxt: ⚠ Context switch — previous session is isolated (not recorded). Continue: %s\n", b.ResumeCmd)
		} else if len(superseded) > 0 {
			fmt.Println("cxt: ⚠ Context switch — previous session is isolated (not recorded)")
		}
		boundary.Notify("cxt Context switch", fmt.Sprintf("%s → %s — previous session isolated, new context prepared", b.PrevBranch, b.Branch))
		if len(superseded) > 0 && remotecfg.BoundaryEnforce(cwd) == "kill" && wrapperManaged {
			// Detach session agent delay end — detached helper (hook can be a descendant of the agent, so it cannot be killed immediately). The wrapper (cxt claude) detects child death and automatically restarts the seed.
			if exe, err := os.Executable(); err == nil {
				cmd := exec.Command(exe, "git-hook", "boundary-enforce")
				cmd.Dir = cwd
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				_ = cmd.Start()
			}
		} else if len(superseded) > 0 && remotecfg.BoundaryEnforce(cwd) == "kill" {
			hookWarn("no live owning cxt wrapper — active agent was not terminated; continue explicitly with %s", b.ResumeCmd)
		}
	}
	return nil
}

// supervisedProvider returns the provider owned by the live cxt wrapper in the
// current process ancestry. A boolean environment marker alone is insufficient:
// resumed shells and app integrations can retain CXT_WRAPPED after the original
// supervisor has exited, which previously caused an unrestartable boundary kill.
func supervisedProvider() (domain.ProviderKind, bool) {
	agent := domain.ProviderKind(os.Getenv("CXT_WRAPPED_AGENT"))
	if agent != domain.ProviderClaude && agent != domain.ProviderCodex {
		agent = domain.ProviderClaude
	}
	if os.Getenv("CXT_WRAPPED") != "1" {
		return agent, false
	}
	wrapperPID, err := strconv.Atoi(os.Getenv("CXT_WRAPPER_PID"))
	if err != nil || wrapperPID <= 1 {
		return agent, false
	}
	return agent, hasProcessAncestor(os.Getppid(), wrapperPID, processParentPID)
}

func processParentPID(pid int) (int, bool) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return ppid, err == nil && ppid > 0
}

func hasProcessAncestor(start, want int, parent func(int) (int, bool)) bool {
	if start <= 1 || want <= 1 {
		return false
	}
	seen := map[int]bool{}
	for pid, depth := start, 0; pid > 1 && depth < 64 && !seen[pid]; depth++ {
		if pid == want {
			return true
		}
		seen[pid] = true
		next, ok := parent(pid)
		if !ok {
			return false
		}
		pid = next
	}
	return false
}

// hookLoadMode is the load fidelity of the hook path (no flag): local load.mode > server personal setting.
func hookLoadMode(cwd string) string {
	if v := remotecfg.LoadMode(cwd); v != "" {
		return v
	}
	return serverLoadMode(cwd)
}

// claudeSessionFiles returns all Claude session files (*.jsonl) in the cwd (for isolation).
func claudeSessionFiles(cwd string) []string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	root, err := providerfs.ClaudeProjectsDir()
	if err != nil {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(root, providerfs.EncodeCwd(abs), "*.jsonl"))
	return matches
}

// codexSessionFiles returns all Codex rollout session files (*.jsonl) in the cwd (for isolation, symmetric to claude).
func codexSessionFiles(ctx context.Context, cwd string) []string {
	return capture.NewCodexCapture().SessionFilesForCwd(ctx, cwd)
}

// restoredSessionID returns the canonical provider-rewritten resume target.
// Materialization must have produced both a path and a trusted resume command.
// For branch seeds, materializedID cross-checks the use-case output against the
// command so a stale synthetic CIR ID cannot become the wrapper restart target.
func restoredSessionID(path, resumeCmd, materializedID string) string {
	if path == "" || resumeCmd == "" {
		return ""
	}
	fields := strings.Fields(resumeCmd)
	if len(fields) == 0 {
		return ""
	}
	resumeID := fields[len(fields)-1]
	if !providerfs.ValidSessionID(resumeID) {
		return ""
	}
	if materializedID != "" && (!providerfs.ValidSessionID(materializedID) || materializedID != resumeID) {
		return ""
	}
	return resumeID
}

// boundaryTransitionSafe prevents a live wrapper from observing a boundary
// that cannot restart its child. An unmanaged superseded-only boundary remains
// recordable for audit and explicit enforcement.
func boundaryTransitionSafe(b boundary.Boundary, wrapperManaged bool) bool {
	restartable := b.SeedPath != "" && b.ResumeCmd != "" && providerfs.ValidSessionID(b.SeedID)
	preparedResume := b.SeedPath != "" || b.ResumeCmd != "" || b.SeedID != ""
	return restartable || (!wrapperManaged && !preparedResume)
}

// runGitHook handles `cxt git-hook <event> [args...]`. Always returns nil (fail-open).
// Total limit: 60 seconds: Hooks block git commands, so they must finish in finite time for network operations.
// (Individual HTTPs have a 30-second client timeout as a first defense — this is the total safety net).
func runGitHook(ctx context.Context, c *Container, cwd string, rest []string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if len(rest) == 0 {
		return nil
	}
	event, args := rest[0], rest[1:]

	switch event {
	case "post-commit":
		// git commit → context snapshot. Passes the message and SHA directly to link code ↔ context.
		msg := gitOut(cwd, "log", "-1", "--pretty=%s")
		sha := gitOut(cwd, "rev-parse", "--short", "HEAD")
		if sha != "" {
			msg = fmt.Sprintf("%s [git %s]", msg, sha)
		}
		_, _ = snapshotForCommit(ctx, c, cwd, msg)

	case "pending-sync":
		// Reflects the in-progress context pointer to the server (detached helper — resolves hook capture/commit after spawn).
		// --resolve <sessionID> deletes the remote pending session associated with the commit.
		if _, ok := remotecfg.Origin(cwd); !ok && os.Getenv("CXT_REMOTE") == "" {
			return nil
		}
		var resolve []string
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--resolve" {
				resolve = append(resolve, args[i+1])
			}
		}
		release := acquireSyncLock(cwd)
		defer release()
		if _, err := c.Sync.SyncPendings(ctx, inbound.SyncInput{Cwd: cwd}, resolve); err != nil {
			hookWarn("pending sync: %v", err)
		}

	case "post-checkout":
		// args: <prev-HEAD> <new-HEAD> <flag>. flag=1 for branch checkout (0 for file checkout — ignored).
		if len(args) < 3 || args[2] != "1" {
			return nil
		}
		return contextSwitch(ctx, c, cwd)

	case "boundary-enforce":
		// Transition execution (detached helper): Waits briefly until the hook/tool call is complete,
		// then terminates the isolated session file agent (POSIX lsof/kill — provider API independent).
		time.Sleep(3 * time.Second)
		if n := boundary.EnforceKill(cwd); n > 0 {
			hookWarn("terminated %d agent(s) from the isolated session to enforce the transition — continue from the seed", n)
		}

	case "pre-push":
		// git push → cxt push. If origin is not registered, only provides instructions (code push continues).
		if _, ok := remotecfg.Origin(cwd); !ok && os.Getenv("CXT_REMOTE") == "" {
			hookWarn("Context not pushed — connect with cxt remote add origin <url>")
			return nil
		}
		out, err := c.Sync.Push(ctx, inbound.SyncInput{Cwd: cwd})
		if err != nil && strings.Contains(err.Error(), domain.ErrSyncConflict.Error()) {
			// A non-fast-forward rejection triggers an automatic append retry. Context does not force replicas
			// to converge (the local lineage is authoritative for this session; pulling is the user's choice), so
			// every divergent push must succeed without loss. The server leaves natural Parents unchanged and adds
			// the remote head as a graft overlay at the new segment boundary, preserving and only expanding reachability.
			out2, aerr := c.Sync.Push(ctx, inbound.SyncInput{Cwd: cwd, Append: true})
			if aerr == nil {
				out, err = out2, nil
				fmt.Println("cxt: repositioned and appended after the remote head — no history lost")
			} else {
				// Expose append failure cause (usually 60s hook timeout — large backlog graft).
				// Remove misleading "just conflict" diagnosis and guide manual recovery.
				err = fmt.Errorf("auto append failed (%v) — run 'cxt push --append' manually", aerr)
			}
		}
		if err != nil {
			syncWarn(cwd, "push", err)
			return nil
		}
		clearAuthHint(cwd)
		fmt.Printf("cxt: pushed %d snapshot(s), %d ref(s) → origin\n", out.Pushed, len(out.NewRefs))

	case "ref-sync":
		// reference-transaction(committed) branch ref change. stdin: "<old> <new> <ref>" lines.
		// Responds only if the current branch ref moves to the final state (HEAD).
		// (Git branch creation/fetch etc. changes are filtered out here).
		branch := gitOut(cwd, "rev-parse", "--abbrev-ref", "HEAD")
		if branch == "" || branch == "HEAD" {
			return nil
		}
		want := "refs/heads/" + branch
		headFull := gitOut(cwd, "rev-parse", "HEAD")
		touched := false
		type created struct{ name, oid string }
		var createdRefs []created
		var deletedRefs []string
		zeros := strings.Repeat("0", 40)
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) != 3 || !strings.HasPrefix(f[2], "refs/heads/") {
				continue
			}
			name := strings.TrimPrefix(f[2], "refs/heads/")
			if f[0] == zeros && f[1] != zeros {
				// branch creation (git branch X / checkout -b X) — context branches too.
				createdRefs = append(createdRefs, created{name: name, oid: f[1]})
				continue
			}
			if f[0] == zeros && f[1] == zeros {
				// branch deletion (git branch -D / PR merge cleanup). Git reports the committed
				// phase of the deletion as zeros→zeros (empirically — old oid does not come).
				// Immutable invariant (P1): context is never deleted or changed — only preservation notice is output.
				deletedRefs = append(deletedRefs, name)
				continue
			}
			if f[2] == want && f[1] == headFull {
				touched = true
			}
		}
		for _, name := range deletedRefs {
			// Context existence check: branch label snapshot or cxt branch ref (includes fork-only).
			n := 0
			if list, lerr := c.List.List(ctx, inbound.ListInput{Branch: name}); lerr == nil {
				n = len(list.Snapshots)
			}
			refExists := false
			if _, err := os.Stat(filepath.Join(cwd, ".cxt", "refs", "heads", filepath.FromSlash(name))); err == nil {
				refExists = true
			}
			if n > 0 || refExists {
				fmt.Printf("cxt: git branch %q deleted — context preserved (restore: cxt checkout %s)\n", name, name)
			}
		}
		if !operationInProgress(cwd) {
			// delayed handling by detached helper: checkout -b/switch -c (transient creation) temporarily
			// leaves HEAD on that branch, so the helper exits to adhere to the seed principle (always seed),
			// and only performs web fork connections/local branches without switching.
			for _, cr := range createdRefs {
				spawnForkConnect(cwd, cr.name, cr.oid)
			}
		}
		if !touched {
			return nil
		}
		refSync(ctx, c, cwd)

	case "fork-connect":
		// context branch (detached helper) for git branch <name> (transient creation).
		// priority: ④ remote with same name (web fork) connects/sorts, else local [git sha] branch.
		if len(args) < 1 {
			return nil
		}
		name := args[0]
		oid := ""
		if len(args) > 1 {
			oid = args[1]
		}
		// transient creation (checkout -b/switch -c) completes sibling post-checkout (seed) soon.
		// polls for seed signal (HEAD==name or .cxt ref creation), exits immediately upon signal — fast machines
		// complete in ~50ms, slower machines wait until deadline (fixed 1.2s issue with seed-preemption in backlog #3).
		// this helper is detached (setsid), so it doesn't block git commands. a pure git branch (no switch) waits
		// until deadline, then proceeds to local branch (normal path). the deadline is an upper bound for "this
		// time, if no seed signal, transient git branch confirmed". transient creation typically completes
		// early, so the upper bound only delays pure git branch (web fork connections etc.) — fixed slippage (1.2s)
		// is maintained.
		ctxRefPath := filepath.Join(cwd, ".cxt", "refs", "heads", filepath.FromSlash(name))
		deadline := time.Now().Add(1500 * time.Millisecond)
		for {
			if gitOut(cwd, "rev-parse", "--abbrev-ref", "HEAD") == name {
				return nil // checkout -b/switch -c — seed principle respected
			}
			if _, err := os.Stat(ctxRefPath); err == nil {
				return nil // context branch already exists (seed/existing) — idempotent
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if gitOut(cwd, "rev-parse", "--verify", "refs/heads/"+name) == "" {
			return nil // branch disappeared in between
		}
		if _, ok := remotecfg.Origin(cwd); ok || os.Getenv("CXT_REMOTE") != "" {
			if ref, err := c.Sync.ResolveRemoteBranch(ctx, inbound.SyncInput{Cwd: cwd}, name); err == nil && ref.Target != "" {
				connectWebFork(ctx, c, cwd, name, ref.Target)
				return nil
			}
		}
		forkContextForBranch(ctx, c, cwd, name, oid)

	case "post-rewrite":
		// rebase/amend complete. stdin: "<old-sha> <new-sha>" mapping — accumulated in side table,
		// then synchronizes context to HEAD at rewrite completion.
		kind := "rebase"
		if len(args) > 0 {
			kind = args[0]
		}
		rewrites := loadRewrites(cwd)
		added := 0
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) >= 2 && f[0] != f[1] {
				rewrites[f[0]] = f[1]
				added++
			}
		}
		if added > 0 {
			if err := saveRewrites(cwd, rewrites); err != nil {
				hookWarn("rewrite log failure: %v", err)
			} else {
				fmt.Printf("cxt: %s — recorded %d commit rewrites ([git <sha>] links preserved)\n", kind, added)
			}
		}
		refSync(ctx, c, cwd)
		// git pull --rebase (pull.rebase=true team default) goes here instead of post-merge —
		// same context handling as merge pull (fetch·rebase·merge promotion). Amend excluded
		// (not an input — ORIG_HEAD remains unchanged, allowing stale range to be read).
		if kind == "rebase" {
			handleIncomingContexts(ctx, c, cwd)
		}

	case "stash-sync":
		// git stash push/pop detected (reference-transaction hook) → syncs cxt stack with git stack depth.
		// No need to distinguish push/pop, and missed events are resolved in the next invocation.
		gitDepth := 0
		if out := gitOut(cwd, "rev-list", "--walk-reflogs", "--count", "refs/stash"); out != "" {
			fmt.Sscanf(out, "%d", &gitDepth)
		}
		cxtStack, err := c.Stash.StashList(ctx, cwd)
		if err != nil {
			return nil
		}
		for gitDepth > len(cxtStack) {
			// git is deeper → stash push occurred. Message inherits from latest git stash item.
			msg := gitOut(cwd, "log", "-g", "-1", "--format=%gs", "refs/stash")
			out, serr := c.Stash.Stash(ctx, inbound.StashInput{Cwd: cwd, Message: msg, Author: c.Identity})
			if serr != nil {
				if serr != domain.ErrNoActiveSession {
					hookWarn("context stash failure: %v", serr)
				}
				return nil
			}
			fmt.Printf("cxt: stashed context (%s) — restored to %q head context\n", shortHash(out.StashID), out.Branch)
			if out.ResumeCmd != "" {
				fmt.Printf("  → resume: %s\n", out.ResumeCmd)
			}
			cxtStack, _ = c.Stash.StashList(ctx, cwd)
		}
		for gitDepth < len(cxtStack) {
			// git is shallower → stash pop/drop occurred. Restores preserved context.
			out, perr := c.Stash.StashPop(ctx, cwd)
			if perr != nil {
				return nil
			}
			fmt.Printf("cxt: restored stashed context (%s)\n", shortHash(out.Entry.Snapshot))
			if out.ResumeCmd != "" {
				fmt.Printf("  → resume: %s\n", out.ResumeCmd)
			}
			cxtStack, _ = c.Stash.StashList(ctx, cwd)
		}

	case "post-merge":
		// git pull/merge → cxt fetch(only objects). If no origin, do nothing silently.
		// Unlike code, context does not force convergence: local refs (history) are maintained,
		// and a hint is shown if new context exists on the remote — fetching is user-selected (cxt pull/load).
		handleIncomingContexts(ctx, c, cwd)
	}
	return nil
}

// handleIncomingContexts processes team contexts incoming through code merge/merge/pull/pull --rebase:
// fetch(only objects) → remote hint → briefing creation → context promotion of merge commit.
// Called from post-merge (merge pull) and post-rewrite (rebase pull — pull.rebase=true team default path).
// If origin is not registered, do nothing silently.
func handleIncomingContexts(ctx context.Context, c *Container, cwd string) {
	if _, ok := remotecfg.Origin(cwd); !ok && os.Getenv("CXT_REMOTE") == "" {
		return
	}
	out, err := c.Sync.Pull(ctx, inbound.SyncInput{Cwd: cwd, FetchOnly: true})
	if err != nil {
		syncWarn(cwd, "pull", err)
		return
	}
	clearAuthHint(cwd)
	if out.Pulled > 0 {
		fmt.Printf("cxt: fetched %d snapshot(s) from origin (local history maintained)\n", out.Pulled)
	}
	for _, b := range out.RemoteAhead {
		hookWarn("New context on remote %q — history move is 'cxt pull', session injection is 'cxt load'", b)
	}
	// If team member context is injected into the current branch, create a briefing — raw does not merge
	// (non-convergent policy) but summarizes the next prompt hook as additionalContext to the live session.
	branch := gitOut(cwd, "rev-parse", "--abbrev-ref", "HEAD")
	for _, b := range out.RemoteAhead {
		if b == branch && branch != "" && branch != "HEAD" {
			writePullBriefing(ctx, c, cwd, branch)
			break
		}
	}
	// PR merge context promotion: First resolve host-side squash/rebase/merge
	// commits back to their source branches. Then retain the generic [git sha]
	// path for non-GitHub and direct merge histories.
	shas := incomingCommitSHAs(cwd)
	appendMergedPRContexts(ctx, c.PRMerges, c.Sync, cwd, branch, gitOut(cwd, "config", "--get", "remote.origin.url"), shas)
	appendMergedContexts(ctx, c, cwd, branch, shas)
}

func incomingCommitSHAs(cwd string) []string {
	if gitOut(cwd, "rev-parse", "--verify", "-q", "ORIG_HEAD") == "" {
		return nil // not merge/pull path (initial clone, etc.)
	}
	raw := gitOut(cwd, "rev-list", "--reverse", "ORIG_HEAD..HEAD")
	if raw == "" {
		return nil
	}
	shas := strings.Fields(raw) // oldest first
	if len(shas) > 200 {
		shas = shas[len(shas)-200:] // large merge defense — last 200 only
	}
	return shas
}

type mergedPRContextSync interface {
	ResolveRemoteBranch(ctx context.Context, in inbound.SyncInput, branch string) (domain.Ref, error)
	AppendBranch(ctx context.Context, in inbound.SyncInput, branch string, target domain.ContentHash) error
}

// appendMergedPRContexts promotes each merged PR source tip to the current base
// branch in Git commit order. Discovery failure is fail-open because this runs
// inside a Git hook; the hosted signed webhook remains the primary path.
func appendMergedPRContexts(
	ctx context.Context,
	resolver outbound.PullRequestMergeResolver,
	syncer mergedPRContextSync,
	cwd, branch, gitRemoteURL string,
	shas []string,
) {
	if resolver == nil || syncer == nil || branch == "" || branch == "HEAD" || gitRemoteURL == "" || len(shas) == 0 {
		return
	}
	pulls, err := resolver.ResolveMergedPullRequests(ctx, gitRemoteURL, branch, shas)
	if err != nil {
		hookWarn("GitHub PR context lookup failed (git continues): %v", err)
		return
	}

	appended := 0
	for _, pull := range pulls {
		if pull.BaseBranch != branch || pull.HeadBranch == "" || pull.HeadBranch == branch {
			continue
		}
		ref, rerr := syncer.ResolveRemoteBranch(ctx, inbound.SyncInput{Cwd: cwd}, pull.HeadBranch)
		if rerr != nil {
			if !errors.Is(rerr, domain.ErrNotFound) {
				hookWarn("PR #%d context branch %q lookup failed: %v", pull.Number, pull.HeadBranch, rerr)
			}
			continue
		}
		if ref.Target == "" {
			continue
		}
		if aerr := syncer.AppendBranch(ctx, inbound.SyncInput{Cwd: cwd}, branch, ref.Target); aerr != nil {
			if strings.Contains(aerr.Error(), "non_fast_forward") {
				continue // hosted webhook or another client already promoted it
			}
			hookWarn("PR #%d context promotion failed (%s → %s): %v", pull.Number, pull.HeadBranch, branch, aerr)
			continue
		}
		appended++
	}
	if appended > 0 {
		fmt.Printf("cxt: promoted %d merged PR context(s) to %q timeline (appended)\n", appended, branch)
	}
}

// appendMergedContexts appends git merge/pull commits and chained context snapshots to the same-named cxt branch.
//
// In the PR flow, context accumulates on feature branches and merge happens on git host, so cxt main becomes the permanent anchor
// — this hook fills that gap. Append is a lossless operation (overlay) that is idempotent and conflict-free (server CAS,
// non-ff targets rejected → skip). Provider PR resolution above supplies the
// squash/rebase path whose original commit links are absent from the base Git history.
func appendMergedContexts(ctx context.Context, c *Container, cwd, branch string, shas []string) {
	if branch == "" || branch == "HEAD" || len(shas) == 0 {
		return
	}
	list, err := c.List.List(ctx, inbound.ListInput{})
	if err != nil {
		return
	}
	rewrites := loadRewrites(cwd)
	// [git <sha>] link → snapshot. Multiple snapshots for the same commit are the latest (same as refSync).
	type linked struct {
		sha  string
		snap domain.Snapshot
	}
	var links []linked
	for _, snap := range list.Snapshots { // newest first
		m := gitLinkRe.FindStringSubmatch(snap.Message)
		if m == nil {
			continue
		}
		links = append(links, linked{sha: resolveRewritten(rewrites, m[1]), snap: snap})
	}
	byID := map[domain.ContentHash]domain.Snapshot{}
	for _, s := range list.Snapshots {
		byID[s.ID] = s
	}
	// Collect candidates in insertion commit order (oldest first) — ensure final tip is the latest merge.
	seen := map[domain.ContentHash]bool{}
	var cands []domain.Snapshot
	for _, sha := range shas {
		for _, l := range links {
			if l.sha == "" || !strings.HasPrefix(sha, l.sha) {
				continue
			}
			if !seen[l.snap.ID] {
				seen[l.snap.ID] = true
				cands = append(cands, l.snap)
			}
			break // only the latest snapshot
		}
	}
	if len(cands) == 0 {
		return
	}
	// Remove candidates already reachable from the server branch + ancestors of other candidates (minimize requests).
	reach := map[domain.ContentHash]bool{}
	walk := func(from domain.ContentHash) {
		stack := []domain.ContentHash{from}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if cur == "" || reach[cur] {
				continue
			}
			reach[cur] = true
			if sn, ok := byID[cur]; ok {
				stack = append(stack, sn.ReachabilityParents()...)
			}
		}
	}
	if ref, rerr := c.Sync.ResolveRemoteBranch(ctx, inbound.SyncInput{Cwd: cwd}, branch); rerr == nil && ref.Target != "" {
		walk(ref.Target)
	}
	var kept []domain.Snapshot
	for i := len(cands) - 1; i >= 0; i-- { // newest candidates first — absorb ancestor candidates as cover
		if reach[cands[i].ID] {
			continue
		}
		kept = append([]domain.Snapshot{cands[i]}, kept...)
		walk(cands[i].ID)
	}
	appended := 0
	for _, sn := range kept { // from oldest — tip = latest merge
		if aerr := c.Sync.AppendBranch(ctx, inbound.SyncInput{Cwd: cwd}, branch, sn.ID); aerr != nil {
			if strings.Contains(aerr.Error(), "non_fast_forward") {
				continue // skip if another team member has already promoted (idempotent no-op)
			}
			hookWarn("merge context promotion failed(%s): %v", shortHash(sn.ID), aerr)
			continue
		}
		appended++
	}
	if appended > 0 {
		fmt.Printf("cxt: promoted %d merge contexts to %q timeline (appended)\n", appended, branch)
	}
}

// --- History Rewrite Mapping (.cxt/rewrites.json) ---
// Snapshot message [git <sha>] links are immutable (content-addressed), so they cannot be modified.
// Similar to git's refs/replace, we accumulate old→new commit mappings in a side table and follow the chain when interpreting links (aaa→bbb→ccc).

func rewritesPath(cwd string) string { return filepath.Join(cwd, ".cxt", "rewrites.json") }

func loadRewrites(cwd string) map[string]string {
	m := map[string]string{}
	if b, err := providerfs.ReadRepoFile(cwd, filepath.Join(".cxt", "rewrites.json")); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func saveRewrites(cwd string, m map[string]string) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return providerfs.WriteRepoFileAtomic(cwd, filepath.Join(".cxt", "rewrites.json"), b, 0o644)
}

// resolveRewritten interprets the final SHA in the rewrite chain for a given sha (short form possible).
func resolveRewritten(m map[string]string, sha string) string {
	cur := sha
	for i := 0; i < 100; i++ { // prevent loop
		next := ""
		for old, nw := range m {
			if strings.HasPrefix(old, cur) || strings.HasPrefix(cur, old) {
				next = nw
				break
			}
		}
		if next == "" || next == cur {
			return cur
		}
		cur = next
	}
	return cur
}

var gitLinkRe = regexp.MustCompile(`\[git ([0-9a-f]{4,40})\]`)

// operationInProgress determines if rebase/merge/cherry-pick is in progress.
// reference-transaction fires even during operations, so do not modify the context during them (completion signals are provided by post-rewrite/post-merge).
func operationInProgress(cwd string) bool {
	for _, p := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "BISECT_LOG"} {
		gp := gitOut(cwd, "rev-parse", "--git-path", p)
		if gp == "" {
			continue
		}
		if !filepath.IsAbs(gp) {
			gp = filepath.Join(cwd, gp)
		}
		if _, err := os.Stat(gp); err == nil {
			return true
		}
	}
	return false
}

// refSync finds the snapshot linked to the current git HEAD commit and restores the context.
// When the code moves to a different point with commands like reset --hard, the context follows.
// If no matching snapshot is found (e.g., due to a broken commit), it quietly does nothing.
func refSync(ctx context.Context, c *Container, cwd string) {
	if operationInProgress(cwd) {
		return
	}
	branch := gitOut(cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" || branch == "HEAD" {
		return
	}
	headFull := gitOut(cwd, "rev-parse", "HEAD")
	if headFull == "" {
		return
	}
	rewrites := loadRewrites(cwd)
	list, err := c.List.List(ctx, inbound.ListInput{Branch: branch})
	if err != nil {
		return
	}
	for _, snap := range list.Snapshots { // latest first — multiple snapshots for the same commit are adopted the latest
		m := gitLinkRe.FindStringSubmatch(snap.Message)
		if m == nil {
			continue
		}
		resolved := resolveRewritten(rewrites, m[1])
		if strings.HasPrefix(headFull, resolved) || strings.HasPrefix(resolved, headFull) {
			out, lerr := c.Load.Load(ctx, inbound.LoadInput{Ref: string(snap.ID), Cwd: cwd})
			if lerr != nil {
				return
			}
			fmt.Printf("cxt: restored context linked to HEAD (%s) from %s\n", headFull[:7], shortHash(snap.ID))
			if out.ResumeCmd != "" {
				fmt.Printf("  → resume: %s\n", out.ResumeCmd)
			}
			syncSettingsToSnapshot(ctx, c, cwd, snap.ID, "reset/rebase → "+headFull[:7])
			return
		}
	}
}

// forkContextForBranch creates a context branch along with a git branch when it is created (without checkout).
// Branch base = the commit the new branch points to and the linked snapshot ([git <sha>]) — if none, the latest of the current branch.
// A ref is created (the active session is not affected — git branch is a background task),
// and if the cxt branch already exists (e.g., due to a team context pull), it is preserved.
// writePullBriefing summarizes the team member snapshots received via git pull (remote head → local head) into a briefing sidecar.
// The next prompt's UserPromptSubmit hook consumes this once, injecting it into the live agent session (no raw merge — summary layer at commit message and author level).
func writePullBriefing(ctx context.Context, c *Container, cwd, branch string) {
	ref, err := c.Sync.ResolveRemoteBranch(ctx, inbound.SyncInput{Cwd: cwd}, branch)
	if err != nil || ref.Target == "" {
		return
	}
	all, err := c.List.List(ctx, inbound.ListInput{})
	if err != nil {
		return
	}
	byID := map[domain.ContentHash]*domain.Snapshot{}
	for i := range all.Snapshots {
		byID[all.Snapshots[i].ID] = &all.Snapshots[i]
	}
	var localTarget domain.ContentHash
	for _, r := range all.Refs {
		if r.Kind == domain.RefBranch && r.Name == branch {
			localTarget = r.Target
			break
		}
	}
	// The "ingestion range" is from the remote head to the local head (latest first, max 12).
	var items []string
	seen := map[domain.ContentHash]bool{}
	for cur := ref.Target; cur != "" && cur != localTarget && !seen[cur] && len(items) < 12; {
		seen[cur] = true
		s := byID[cur]
		if s == nil {
			break // object not reached (fetch failure etc.) — only what is available
		}
		if !strings.HasPrefix(s.Message, domain.HookMessagePrefix) {
			who := s.Author.Name
			if who == "" {
				who = s.Author.Email
			}
			if who == "" {
				who = s.Provider
			}
			items = append(items, fmt.Sprintf("- %s: %s", who, s.Message))
		}
		if len(s.Parents) == 0 {
			break
		}
		cur = s.Parents[0]
	}
	if len(items) == 0 {
		return
	}
	text := fmt.Sprintf("── Teammate context arrived (git pull, %s) ──\n%s\n(full conversations are available in the web context tab — the code changes are already in your working tree)",
		branch, strings.Join(items, "\n"))
	if capture.WriteBriefing(cwd, text) == nil {
		fmt.Printf("cxt: %d team member contexts received — will be briefed to the agent in the next prompt\n", len(items))
	}
}

// spawnForkConnect spawns the fork-connect detached helper (ref-sync runs a git command in the middle of speech,
// so the ref is not modified there — delayed post-processing by the detached process).
func spawnForkConnect(cwd, branch, oid string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "git-hook", "fork-connect", branch, oid)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Start()
}

// connectWebFork connects a web fork branch locally (user agreement: the fork's base is a git commit).
//
// Fork snapshot [git X] parsing → git branch -f <name> X (aligns code to the fork point)
//
//	→ local context ref <name> = fork snapshot (context follows)
//
// Subsequent git switch <name> materializes the existing-branch path with this context — code=X, context=S@X.
// If X is not in local git (e.g., someone's On Hold fork), the connection is deferred and a notice is given.
func connectWebFork(ctx context.Context, c *Container, cwd, branch string, target domain.ContentHash) {
	all, err := c.List.List(ctx, inbound.ListInput{})
	if err != nil {
		return
	}
	var snap *domain.Snapshot
	for i := range all.Snapshots {
		if all.Snapshots[i].ID == target {
			snap = &all.Snapshots[i]
			break
		}
	}
	if snap == nil {
		return // fetch failure etc. — try again later
	}
	m := gitLinkRe.FindStringSubmatch(snap.Message)
	if m == nil {
		hookWarn("Web fork %q connection deferred — no [git sha] link in fork snapshot (save manually at the snapshot point)", branch)
		return
	}
	sha := resolveRewritten(loadRewrites(cwd), m[1])
	if gitOut(cwd, "cat-file", "-t", sha) != "commit" {
		hookWarn("Web fork %q connection deferred — fork commit [git %s] not found locally (push to git and then fetch)", branch, sha)
		return
	}
	if out, gerr := exec.Command("git", "-C", cwd, "branch", "-f", branch, sha).CombinedOutput(); gerr != nil {
		hookWarn("Web fork %q code alignment failed: %s", branch, strings.TrimSpace(string(out)))
		return
	}
	if _, ferr := c.Fork.Fork(ctx, inbound.ForkInput{RepoID: snap.RepoID, FromSnapshot: target, NewBranch: branch, Author: c.Identity}); ferr != nil {
		hookWarn("Web fork %q context connection failed: %v", branch, ferr)
		return
	}
	fmt.Printf("cxt: Web fork %q connected — branch aligned to fork point [git %s], context %s\n", branch, sha, shortHash(target))
	boundary.Notify("cxt Web fork connection", fmt.Sprintf("%s → [git %s] aligned — start with git switch %s", branch, sha, branch))
}

func forkContextForBranch(ctx context.Context, c *Container, cwd, branch, commitOID string) {
	rewrites := loadRewrites(cwd)
	all, err := c.List.List(ctx, inbound.ListInput{})
	if err != nil || len(all.Snapshots) == 0 {
		return
	}
	var from domain.ContentHash
	var repoID string
	for _, snap := range all.Snapshots {
		if snap.Branch == domain.StashBranchLabel {
			continue
		}
		m := gitLinkRe.FindStringSubmatch(snap.Message)
		if m == nil {
			continue
		}
		resolved := resolveRewritten(rewrites, m[1])
		if strings.HasPrefix(commitOID, resolved) || strings.HasPrefix(resolved, commitOID) {
			from, repoID = snap.ID, snap.RepoID
			break
		}
	}
	if from == "" {
		// fallback: current branch's latest context (same HEAD as general git branch X case).
		cur := gitOut(cwd, "rev-parse", "--abbrev-ref", "HEAD")
		if cur == "" || cur == "HEAD" {
			return
		}
		list, lerr := c.List.List(ctx, inbound.ListInput{Branch: cur})
		if lerr != nil || len(list.Snapshots) == 0 {
			return
		}
		from, repoID = list.Snapshots[0].ID, list.Snapshots[0].RepoID
	}
	out, ferr := c.Fork.Fork(ctx, inbound.ForkInput{RepoID: repoID, FromSnapshot: from, NewBranch: branch, Author: c.Identity})
	if ferr != nil {
		return // already exists (ErrBranchExists) — silently preserve like git
	}
	fmt.Printf("cxt: context branch %q created (head %s)\n", out.Branch, shortHash(out.Head))
}
