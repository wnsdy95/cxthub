package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func branchOperationCandidate(line string) (branch, oid string, orphan bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return
	}
	if fields[2] == "HEAD" && isZeroGitOID(fields[0]) && strings.HasPrefix(fields[1], "ref:refs/heads/") {
		return strings.TrimPrefix(fields[1], "ref:refs/heads/"), "", true
	}
	txn := parseBranchRefTransaction(line)
	if txn.createdOID != "" {
		return txn.name, txn.createdOID, false
	}
	return
}

func isZeroGitOID(value string) bool {
	return (len(value) == 40 || len(value) == 64) && strings.Trim(value, "0") == ""
}

func runBranchTransaction(ctx context.Context, c *Container, cwd string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	phase := args[0]
	gitPID := ""
	if len(args) > 1 {
		gitPID = args[1]
	}
	if root := gitOut(cwd, "rev-parse", "--show-toplevel"); root != "" {
		cwd = root
	}
	j, err := branchjournal.Open(ctx, cwd)
	if err != nil {
		return err
	}
	registered, err := j.Status()
	if err != nil {
		return err
	}
	state := gitctx.InspectContextRoot(ctx, cwd)
	if !registered && !state.Initialized {
		return nil
	}
	var lines []string
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		branch, _, symbolic := branchOperationCandidate(line)
		// Recent Git versions report ordinary symbolic HEAD changes using a
		// zero old object ID too. Only an absent native branch is unborn.
		if symbolic && gitOut(cwd, "rev-parse", "--verify", "refs/heads/"+branch) != "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	hasBirth := false
	for _, line := range lines {
		b, _, _ := branchOperationCandidate(line)
		if b != "" {
			hasBirth = true
		}
	}
	if !hasBirth {
		return nil
	}
	if c.History == nil {
		return fmt.Errorf("CXTHub branch creation stopped: history service unavailable")
	}
	if !state.Initialized {
		return fmt.Errorf("CXTHub branch creation stopped: registered repository replica .cxt is missing or damaged; restore it from a verified backup/server before retrying (journal is preserved in the Git directory)")
	}
	if !registered {
		if err := j.Enable(); err != nil {
			return err
		}
	}
	err = j.Transaction(ctx, func() error {
		ops, err := j.List()
		if err != nil {
			return err
		}
		for _, line := range lines {
			branch, oid, orphan := branchOperationCandidate(line)
			if branch == "" {
				continue
			}
			if err := domain.ValidateBranchName(branch); err != nil {
				return err
			}
			var op *branchjournal.Operation
			// Every prepared callback starts a new transaction. A PID can be
			// reused, and one Git process can perform several ref transactions.
			// A terminal callback must identify exactly one outstanding vote;
			// timestamps cannot disambiguate callbacks after PID reuse.
			if phase != "prepared" {
				for i := len(ops) - 1; i >= 0; i-- {
					candidate := &ops[i]
					if candidate.GitRef == "refs/heads/"+branch && candidate.Event.GitAfter == oid && candidate.Worktree == cwd && candidate.GitPID == gitPID && (candidate.Phase == "prepared" || candidate.Phase == "committed") {
						if op != nil {
							return fmt.Errorf("ambiguous Git transaction for %s (PID %s); recorded votes remain unchanged", branch, gitPID)
						}
						op = candidate
					}
				}
			}
			if phase == "prepared" && op == nil {
				event, err := prepareBranchHistory(ctx, c, cwd, branch, oid, orphan, gitPID)
				if err != nil {
					return err
				}
				if err := j.Bind(event.RepoID); err != nil {
					return err
				}
				log, err := readBranchLog(cwd, branch)
				if err != nil {
					return err
				}
				ops = append(ops, branchjournal.Operation{Event: event, Phase: "prepared", GitRef: "refs/heads/" + branch, Worktree: cwd, GitPID: gitPID, LogBytes: len(log), LogHash: domain.HashContent(log)})
				op = &ops[len(ops)-1]
			}
			if op == nil {
				continue
			} // no prepared vote: never fabricate a historical birth
			if phase == "committed" {
				op.Phase = "committed"
			}
			if phase == "aborted" {
				op.Phase = "aborted"
			}
			if err := j.Save(*op); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("CXTHub could not durably record branch creation: %w", err)
	}
	if phase == "committed" {
		exe, err := os.Executable()
		if err == nil {
			pid := ""
			if len(args) > 1 {
				pid = args[1]
			}
			cmd := exec.Command(exe, "git-hook", "branch-replay", pid)
			cmd.Dir = cwd
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			_ = cmd.Start()
		}
	}
	return nil
}

func prepareBranchHistory(ctx context.Context, c *Container, cwd, branch, oid string, orphan bool, gitPIDs ...string) (domain.HistoryEvent, error) {
	if err := reconcileCompletedPRPosition(ctx, c, cwd); err != nil {
		return domain.HistoryEvent{}, err
	}
	id, err := branchjournal.NewID()
	if err != nil {
		return domain.HistoryEvent{}, err
	}
	repo, err := remotecfg.Wrap(cwd, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
	if err != nil {
		return domain.HistoryEvent{}, err
	}
	e := domain.HistoryEvent{ID: id, BranchID: id, RepoID: repo.ID, Branch: branch, Kind: "birth", GitBefore: gitOut(cwd, "rev-parse", "--verify", "HEAD"), GitAfter: oid, CreatedAt: time.Now().UTC()}
	wt := sha256.Sum256([]byte(gitOut(cwd, "rev-parse", "--absolute-git-dir")))
	e.WorktreeID = fmt.Sprintf("%x", wt[:16])
	history, err := c.History.ListHistory(ctx, repo.ID)
	if err != nil {
		return e, err
	}
	bindings, err := domain.ProjectContextBranches(history)
	if err != nil {
		return e, err
	}
	e.BindingParent = bindings.Released[branch]
	all, err := c.List.List(ctx, inbound.ListInput{RepoID: repo.ID})
	if err != nil {
		return e, err
	}
	current := gitOut(cwd, "symbolic-ref", "--short", "HEAD")
	currentBinding, err := c.History.ResolveLocalBranch(ctx, repo.ID, current)
	if err != nil {
		return e, err
	}
	// Persist command provenance before the prepared vote. Explicit named start
	// refs remain distinct even when they point at the same commit as HEAD.
	var gitPID string
	if len(gitPIDs) > 0 {
		gitPID = gitPIDs[0]
	}
	e.Creation = captureGitCreation(ctx, cwd, gitPID, branch, e.GitBefore, oid, orphan)
	sourceBranch := current
	if e.Creation.Evidence == "process-argv" && e.Creation.OriginBranch != "" {
		origin, err := c.History.ResolveLocalBranch(ctx, repo.ID, e.Creation.OriginBranch)
		if err != nil {
			return e, err
		}
		e.Creation.OriginBranchID = origin.BranchID
		sourceBranch = e.Creation.OriginBranch
		currentBinding = origin
	}
	useWorkingPosition := (oid == e.GitBefore && sourceBranch == current) || orphan
	// For the current code point use the saved working branch. Explicit older
	// start points must resolve via their actual code ancestry, never @{-1}.
	if useWorkingPosition {
		for _, ref := range all.Refs {
			if ref.Kind == domain.RefBranch && ref.Name == currentBinding.Branch {
				e.Source = ref.Target
				break
			}
		}
		if position, err := c.History.CurrentPosition(ctx); err == nil && position.GitBranch() == current {
			e.Source = position.Snapshot
			e.MemoryHash, e.MemoryPinned = position.MemoryHash, position.MemoryPinned
			e.MemorySource = position.MemorySource
		}
	} else if e.Creation.Evidence == "process-argv" && sourceBranch != current && gitOut(cwd, "rev-parse", "--verify", "refs/heads/"+sourceBranch) == oid {
		for _, ref := range all.Refs {
			if ref.Kind == domain.RefBranch && ref.Name == currentBinding.Branch {
				e.Source = ref.Target
				break
			}
		}
		if e.Source == "" && len(all.Snapshots) > 0 {
			return e, fmt.Errorf("no verified context for source branch %s", sourceBranch)
		}
	} else {
		history, err := c.History.ListHistory(ctx, repo.ID)
		if err != nil {
			return e, err
		}
		selected := contextSelectionAtCode(cwd, oid, currentBinding.Branch, all.Snapshots, history)
		selected, err = resolveCompletedPRMemory(ctx, c, currentBinding.Branch, currentBinding.BranchID, selected, history)
		if err != nil {
			return e, err
		}
		e.Source, e.MemoryHash, e.MemoryPinned = selected.Snapshot, selected.MemoryHash, selected.MemoryPinned
		e.MemorySource = selected.MemorySource
		if e.Source == "" && len(all.Snapshots) > 0 {
			return e, fmt.Errorf("no verified context for Git source %s; no branch created", oid)
		}
	}
	// Validate the saved baseline before capturing. A provider failure can fall
	// back to that verified baseline; it must never hide corrupt stored history.
	e.Target = e.Source
	creation := e.Creation
	e.Creation = nil // Baseline proof precedes final orphan/attach classification.
	e, err = c.History.ValidateHistorySource(ctx, e)
	e.Creation = creation
	if err != nil {
		return e, err
	}
	if useWorkingPosition {
		if latest := checkpointBranchSource(ctx, c, cwd, current, branch, e); latest != "" && latest != e.Source {
			e.Source, e.Target, e.MemoryHash = latest, latest, ""
			e.MemorySource = ""
			e.MemoryPinned = false
		}
	}
	if orphan {
		e.Kind = "orphan"
		if e.MemorySource == "" {
			e.MemorySource = e.Source
		}
		e.Source = ""
	}
	e.Target = e.Source
	return c.History.ValidateHistorySource(ctx, e)
}

// Freeze available live bytes before Git commits the branch birth. This step
// does no network I/O and never materializes or terminates a provider session.
func checkpointBranchSource(ctx context.Context, c *Container, cwd, from, to string, baseline domain.HistoryEvent) domain.ContentHash {
	if c.Save == nil || from == "" {
		return ""
	}
	target, err := branchCheckpointTarget(ctx, cwd)
	if err != nil {
		if !errors.Is(err, domain.ErrNoActiveSession) {
			hookWarn("branch source session unavailable; using verified saved context: %v", err)
		}
		return ""
	}
	info, err := os.Lstat(target.SessionPath)
	if err != nil || !info.Mode().IsRegular() || providerfs.CaptureExcluded(cxtRepoRoot(ctx, cwd), target.SessionPath, info.Size()) {
		return ""
	}
	out, err := c.Save.Save(ctx, inbound.SaveInput{Cwd: cwd, Provider: target.Provider, SessionPath: target.SessionPath, Branch: from, Author: c.Identity, Message: "checkpoint: branch " + from + " → " + to})
	if err != nil {
		hookWarn("branch source capture unavailable; using verified saved context: %v", err)
		return ""
	}
	if baseline.Source != "" && baseline.Source != out.SnapshotID {
		// Save reports the captured document even when deduplication preserves
		// a newer selected head. Do not turn that unchanged ancestor into the
		// branch's source or replace the verified baseline's pinned memory.
		ancestor, err := branchCheckpointAncestor(ctx, c, baseline, out.SnapshotID)
		if err != nil {
			hookWarn("branch checkpoint ancestry unavailable; using verified saved context: %v", err)
			return ""
		}
		if ancestor {
			return baseline.Source
		}
	}
	if c.Memorize != nil {
		if _, err := c.Memorize.Memorize(ctx, inbound.MemorizeInput{Cwd: cwd, Provider: target.Provider, Ref: string(out.SnapshotID)}); err != nil {
			hookWarn("branch source memory capture unavailable: %v", err)
		}
	}
	return out.SnapshotID
}

func branchCheckpointAncestor(ctx context.Context, c *Container, baseline domain.HistoryEvent, captured domain.ContentHash) (bool, error) {
	all, err := c.List.List(ctx, inbound.ListInput{RepoID: baseline.RepoID})
	if err != nil {
		return false, err
	}
	snapshots := make(map[domain.ContentHash]domain.Snapshot, len(all.Snapshots))
	for _, snapshot := range all.Snapshots {
		snapshots[snapshot.ID] = snapshot
	}
	seen := map[domain.ContentHash]bool{}
	queue := []domain.ContentHash{baseline.Source}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		snapshot, ok := snapshots[id]
		if !ok || snapshot.RepoID != baseline.RepoID {
			return false, fmt.Errorf("unverified checkpoint ancestor %s", id)
		}
		if id == captured {
			return true, nil
		}
		queue = append(queue, snapshot.ReachabilityParents()...)
	}
	return false, nil
}

func branchCheckpointTarget(ctx context.Context, cwd string) (commandCaptureTarget, error) {
	provider, managed := supervisedProvider(ctx, cwd)
	active := capture.ActiveAppSessions(cwd)
	if managed || strings.TrimSpace(os.Getenv("CODEX_THREAD_ID")) != "" || strings.TrimSpace(os.Getenv("CODEX_SESSION_ID")) != "" {
		// An identified owner takes precedence over every registered sibling.
		// Failure must not fall through to an unidentified registry entry or
		// to file recency.
		id := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID"))
		if id == "" {
			id = strings.TrimSpace(os.Getenv("CODEX_SESSION_ID"))
		}
		if managed {
			id = strings.TrimSpace(os.Getenv("CXT_WRAPPED_SESSION_ID"))
			if !providerfs.ValidSessionID(id) {
				id = capture.SessionAffinity(cwd, provider)
			}
		} else {
			provider = domain.ProviderCodex
		}
		// The official hook already mapped this exact owner to a safe path in
		// this worktree. Preserve its native cwd spelling: Git canonicalizes
		// symlinks, while Claude's session directory encodes the original cwd.
		if providerfs.ValidSessionID(id) {
			for _, session := range active {
				if session.Provider == provider && session.SessionID == id {
					return commandCaptureTarget{Provider: provider, SessionPath: session.Path}, nil
				}
			}
		}
		target, err := commandCapture(ctx, cwd, "")
		if err != nil {
			return commandCaptureTarget{}, err
		}
		if target.SessionPath == "" {
			return commandCaptureTarget{}, fmt.Errorf("command has no verified native session")
		}
		return target, nil
	}
	if len(active) == 0 {
		return commandCaptureTarget{}, domain.ErrNoActiveSession
	}
	if len(active) != 1 {
		return commandCaptureTarget{}, fmt.Errorf("multiple registered sessions; branch command has no exact owner")
	}
	// Hook identities can be opaque. With one live entry in this exact
	// worktree, use its verified path rather than reconstructing a UUID path.
	return commandCaptureTarget{Provider: active[0].Provider, SessionPath: active[0].Path}, nil
}

// Replay uses the durable prepared record and observed Git state. A timeout or
// server failure leaves the operation pending; neither is an acknowledgement.
func replayBranchOperations(ctx context.Context, c *Container, cwd string) error {
	if c.History == nil {
		return nil
	}
	j, err := branchjournal.Open(ctx, cwd)
	if err != nil {
		return err
	}
	enabled, err := j.Status()
	if err != nil || !enabled {
		return err
	}
	var ops []branchjournal.Operation
	err = j.Transaction(ctx, func() error { var err error; ops, err = j.List(); return err })
	if err != nil || len(ops) == 0 {
		return err
	}
	repo, err := remotecfg.Wrap(cwd, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
	if err != nil {
		return err
	}
	if err := j.Transaction(ctx, func() error { return j.Bind(repo.ID) }); err != nil {
		return err
	}
	blocked := map[string]bool{}
	applied := false
	var failures []error
	for _, op := range ops {
		if op.Event.RepoID != repo.ID {
			return domain.ErrHashMismatch
		}
		if op.Phase == "applied" || op.Phase == "aborted" || blocked[op.GitRef] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if op.Phase == "prepared" {
			committed, err := hasBranchCommitWitness(op)
			if err != nil {
				return err
			}
			if !committed {
				// Keep uncertainty in the journal. It is not a birth and must not
				// borrow evidence from, or block, an independently committed vote.
				continue
			}
		}
		// The only network step runs without the prepared-vote lock. A slow remote
		// must not hold up an unrelated Git branch creation. Competing replayers
		// propose a resolution; the first durable resolution wins below.
		event := op.Event
		var resolutionErr error
		if !op.Resolved {
			event, resolutionErr = resolveBranchOperation(ctx, c, cwd, op)
		}
		err = j.Transaction(ctx, func() error {
			current, err := j.List()
			if err != nil {
				return err
			}
			for _, saved := range current {
				if saved.Event.ID != op.Event.ID {
					continue
				}
				if saved.Phase == "applied" || saved.Phase == "aborted" {
					return nil
				}
				// Branch refs belong to the verified shared repository, even if
				// the original worktree has moved or no longer exists. Verify the
				// read before persisting a resolution or acknowledging the operation.
				exists, err := branchRefExists(ctx, cwd, saved.GitRef)
				if err != nil {
					saved.LastError = err.Error()
					if saveErr := j.Save(saved); saveErr != nil {
						return saveErr
					}
					return err
				}
				if !saved.Resolved {
					if resolutionErr != nil {
						saved.LastError = resolutionErr.Error()
						if err := j.Save(saved); err != nil {
							return err
						}
						return resolutionErr
					}
					saved.Event = event
					saved.Resolved = true
					saved.Phase = "committed"
					if err := j.Save(saved); err != nil {
						return err
					}
				}
				if err := applyBranchOperation(ctx, c, cwd, saved, exists); err != nil {
					saved.LastError = err.Error()
					if saveErr := j.Save(saved); saveErr != nil {
						return saveErr
					}
					return err
				}
				saved.Phase = "applied"
				saved.LastError = ""
				if err := j.Save(saved); err != nil {
					return err
				}
				applied = true
				return nil
			}
			return fmt.Errorf("branch operation %s disappeared during replay", op.Event.ID)
		})
		if err != nil {
			blocked[op.GitRef] = true
			failures = append(failures, err)
		}
	}
	if applied && c.Sync != nil {
		if _, ok := remotecfg.Origin(cwd); ok {
			spawnBranchStateSync(cwd)
		}
	}
	return errors.Join(failures...)
}

func resolveBranchOperation(ctx context.Context, c *Container, cwd string, op branchjournal.Operation) (domain.HistoryEvent, error) {
	e := op.Event
	gitDir, err := branchOperationGitDir(ctx, cwd, op)
	if err != nil {
		return e, err
	}
	upstream, err := branchGitOutput(ctx, cwd, "--git-dir="+gitDir, "for-each-ref", "--format=%(upstream)", op.GitRef)
	if err != nil {
		return e, err
	}
	if upstream != "" && e.Kind != "orphan" {
		remoteName, err := branchGitOutput(ctx, cwd, "--git-dir="+gitDir, "config", "--get", "branch."+e.Branch+".remote")
		if err != nil {
			return e, err
		}
		merge, err := branchGitOutput(ctx, cwd, "--git-dir="+gitDir, "config", "--get", "branch."+e.Branch+".merge")
		if err != nil {
			return e, err
		}
		remoteBranch := strings.TrimPrefix(merge, "refs/heads/")
		if remoteName != "" && remoteName != "." && remoteBranch != "" {
			if c.Sync == nil {
				return e, fmt.Errorf("tracking context attachment awaits sync service")
			}
			ref, err := c.Sync.ResolveRemoteBranch(ctx, inbound.SyncInput{Cwd: cwd}, remoteBranch)
			if err != nil {
				return e, fmt.Errorf("tracking context attachment awaits server: %w", err)
			}
			e.Kind = "attach"
			e.Source = ref.Target
			e.Target = ref.Target
			e.SharedTarget = ref.Target
			e.MemoryHash, e.MemoryPinned = "", false
			e.MemorySource = ""
			e.BindingParent = ""
			history, err := c.History.ListHistory(ctx, e.RepoID)
			if err != nil {
				return e, err
			}
			bindings, err := domain.ProjectContextBranches(history)
			if err != nil {
				return e, err
			}
			e.BranchID = bindings.Identity(e.RepoID, remoteBranch)
			if e.Creation != nil && e.Creation.Evidence == "process-argv" && e.Creation.OriginBranch == remoteBranch {
				// The server binding may arrive after the prepared local vote. Resolve
				// this retained identity together with the tracking attachment.
				creation := *e.Creation
				creation.OriginBranchID = e.BranchID
				e.Creation = &creation
			}
			e.LocalBranch, e.Branch = e.Branch, remoteBranch
			all, err := c.List.List(ctx, inbound.ListInput{RepoID: e.RepoID})
			if err != nil {
				return e, err
			}
			selected := contextSelectionAtCode(cwd, e.GitAfter, remoteBranch, all.Snapshots, history)
			selected, err = resolveCompletedPRMemory(ctx, c, remoteBranch, e.BranchID, selected, history)
			if err != nil {
				return e, err
			}
			if selected.Snapshot != "" {
				e.Source, e.Target = selected.Snapshot, selected.Snapshot
				e.MemoryHash, e.MemorySource, e.MemoryPinned = selected.MemoryHash, selected.MemorySource, selected.MemoryPinned
			} else {
				return e, fmt.Errorf("tracking context has no verified association with Git %s; operation remains queued", e.GitAfter)
			}
		}
	}
	return c.History.ValidateHistorySource(ctx, e)
}

// Worktree IDs were frozen from Git's absolute admin directory before birth.
// That directory survives a worktree move and owns config.worktree. The
// replaying worktree is only a route to shared refs, never a source of binding
// configuration. Once the origin admin directory is pruned, an unresolved
// binding cannot be reconstructed safely from another worktree's settings.
func branchOperationGitDir(ctx context.Context, cwd string, op branchjournal.Operation) (string, error) {
	common, err := branchGitOutput(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	matches := func(path string) bool {
		id := sha256.Sum256([]byte(path))
		return fmt.Sprintf("%x", id[:16]) == op.Event.WorktreeID
	}
	if matches(common) {
		return common, nil
	}
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			path := filepath.Join(common, "worktrees", entry.Name())
			if matches(path) {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("origin worktree Git configuration is unavailable for %s; branch binding remains queued", op.Event.WorktreeID)
}

func applyBranchOperation(ctx context.Context, c *Container, cwd string, op branchjournal.Operation, branchExists bool) error {
	e := op.Event
	if err := c.History.RecordHistory(ctx, e); err != nil {
		return err
	}
	if err := c.History.BindLocalBranch(ctx, e); err != nil {
		return err
	}

	if branchExists && e.Kind != "orphan" && e.Target != "" {
		_, err := c.Fork.Fork(ctx, inbound.ForkInput{RepoID: e.RepoID, FromSnapshot: e.Target, NewBranch: e.Branch, Author: c.Identity})
		if err != nil && !errors.Is(err, domain.ErrBranchExists) {
			return err
		}
	}
	wt := sha256.Sum256([]byte(gitOut(cwd, "rev-parse", "--absolute-git-dir")))
	localBranch := strings.TrimPrefix(op.GitRef, "refs/heads/")
	if fmt.Sprintf("%x", wt[:16]) == e.WorktreeID && gitOut(cwd, "symbolic-ref", "--short", "HEAD") == localBranch && gitOut(cwd, "rev-parse", "--verify", "HEAD") == e.GitAfter {
		current, err := c.History.CurrentPosition(ctx)
		if err == nil && current.RepoID == e.RepoID && current.Branch == e.Branch && current.BranchID == e.BranchID {
			return nil
		}
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		p := domain.WorkingPosition{RepoID: e.RepoID, Branch: e.Branch, GitCommit: e.GitAfter, Snapshot: e.Target, MemoryHash: e.MemoryHash, MemoryPinned: e.MemoryPinned, MemorySource: e.MemorySource, Orphan: e.Kind == "orphan"}
		if localBranch != e.Branch {
			p.LocalBranch = localBranch
		}
		if err := c.History.SelectPosition(ctx, p); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	return nil
}

func readBranchLog(cwd, branch string) ([]byte, error) {
	path := gitOut(cwd, "rev-parse", "--git-path", "logs/refs/heads/"+branch)
	if path == "" {
		return nil, fmt.Errorf("cannot resolve branch reflog path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return raw, err
}

func hasBranchCommitWitness(op branchjournal.Operation) (bool, error) {
	// A reflog prefix and zero→OID entry do not identify the transaction that
	// wrote them. The original process may have died before committing and a
	// later creation may reuse both name and OID. Without a durable committed
	// callback (or explicit orphan recovery), prepared records stay unconfirmed.
	return op.Phase == "committed" || op.Phase == "applied", nil
}

func branchRefExists(ctx context.Context, cwd, ref string) (bool, error) {
	// show-ref --verify --quiet returns the same status for missing and broken
	// loose refs. for-each-ref distinguishes an absent ref from unreadable ones
	// through stderr, which branchGitOutput deliberately refuses to discard.
	refs, err := branchGitOutput(ctx, cwd, "for-each-ref", "--format=%(refname)", ref)
	if err != nil {
		return false, err
	}
	for _, name := range strings.Fields(refs) {
		if name == ref {
			return true, nil
		}
	}
	return false, nil
}

func branchGitOutput(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("cannot inspect Git branch state: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stderr.Len() != 0 {
		return "", fmt.Errorf("cannot verify Git branch state: %s", strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
