package cli

import (
	"bufio"
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
			for i := len(ops) - 1; i >= 0; i-- {
				candidate := &ops[i]
				if candidate.Event.Branch == branch && candidate.Event.GitAfter == oid && candidate.Worktree == cwd && candidate.GitPID == gitPID && (candidate.Phase == "prepared" || candidate.Phase == "committed") {
					op = candidate
					break
				}
			}
			if phase == "prepared" && op == nil {
				event, err := prepareBranchHistory(ctx, c, cwd, branch, oid, orphan)
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

func prepareBranchHistory(ctx context.Context, c *Container, cwd, branch, oid string, orphan bool) (domain.HistoryEvent, error) {
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
	// For the current code point use the saved working branch. Explicit older
	// start points must resolve via their actual code ancestry, never @{-1}.
	if oid == e.GitBefore || orphan {
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
	} else {
		history, err := c.History.ListHistory(ctx, repo.ID)
		if err != nil {
			return e, err
		}
		selected := contextSelectionAtCode(cwd, oid, current, all.Snapshots, history)
		e.Source, e.MemoryHash, e.MemoryPinned = selected.Snapshot, selected.MemoryHash, selected.MemoryPinned
		e.MemorySource = selected.MemorySource
		if e.Source == "" && len(all.Snapshots) > 0 {
			return e, fmt.Errorf("no verified context for Git source %s; no branch created", oid)
		}
	}
	// Validate the saved baseline before capturing. A provider failure can fall
	// back to that verified baseline; it must never hide corrupt stored history.
	e.Target = e.Source
	e, err = c.History.ValidateHistorySource(ctx, e)
	if err != nil {
		return e, err
	}
	if oid == e.GitBefore || orphan {
		if latest := checkpointBranchSource(ctx, c, cwd, current, branch); latest != "" {
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
func checkpointBranchSource(ctx context.Context, c *Container, cwd, from, to string) domain.ContentHash {
	if c.Save == nil || from == "" {
		return ""
	}
	type source struct{ provider, path string }
	var sources []source
	for _, active := range capture.ActiveAppSessions(cwd) {
		sources = append(sources, source{string(active.Provider), active.Path})
	}
	if len(sources) == 0 {
		provider, _ := supervisedProvider(ctx, cwd)
		var locator interface {
			LocateActiveSession(context.Context, string) (string, error)
		}
		if provider == domain.ProviderCodex {
			locator = capture.NewCodexCapture()
		} else {
			locator = capture.NewClaudeCapture()
		}
		if path, err := locator.LocateActiveSession(ctx, cwd); err == nil {
			sources = append(sources, source{string(provider), path})
		}
	}
	var latest domain.ContentHash
	for _, source := range sources {
		info, err := os.Lstat(source.path)
		if err != nil || !info.Mode().IsRegular() || providerfs.CaptureExcluded(cxtRepoRoot(ctx, cwd), source.path, info.Size()) {
			continue
		}
		out, err := c.Save.Save(ctx, inbound.SaveInput{Cwd: cwd, Provider: source.provider, SessionPath: source.path, Branch: from, Author: c.Identity, Message: "checkpoint: branch " + from + " → " + to})
		if err != nil {
			hookWarn("branch source capture unavailable; using verified saved context: %v", err)
			continue
		}
		latest = out.SnapshotID
		if c.Memorize != nil {
			if _, err := c.Memorize.Memorize(ctx, inbound.MemorizeInput{Cwd: cwd, Provider: source.provider, Ref: string(latest)}); err != nil {
				hookWarn("branch source memory capture unavailable: %v", err)
			}
		}
	}
	return latest
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
				blocked[op.GitRef] = true
				continue
			}
		}
		// The only network step runs without the prepared-vote lock. A slow remote
		// must not hold up an unrelated Git branch creation. Competing replayers
		// propose a resolution; the first durable resolution wins below.
		event := op.Event
		var resolutionErr error
		if !op.Resolved {
			event, resolutionErr = resolveBranchOperation(ctx, c, op)
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
				exists := gitOut(saved.Worktree, "rev-parse", "--verify", saved.GitRef) != ""
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

func resolveBranchOperation(ctx context.Context, c *Container, op branchjournal.Operation) (domain.HistoryEvent, error) {
	e := op.Event
	upstream := gitOut(op.Worktree, "for-each-ref", "--format=%(upstream)", op.GitRef)
	if upstream != "" && e.Kind != "orphan" {
		remoteName := gitOut(op.Worktree, "config", "--get", "branch."+e.Branch+".remote")
		remoteBranch := strings.TrimPrefix(gitOut(op.Worktree, "config", "--get", "branch."+e.Branch+".merge"), "refs/heads/")
		if remoteName != "" && remoteName != "." && remoteBranch != "" {
			ref, err := c.Sync.ResolveRemoteBranch(ctx, inbound.SyncInput{Cwd: op.Worktree}, remoteBranch)
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
			e.LocalBranch, e.Branch = e.Branch, remoteBranch
			all, err := c.List.List(ctx, inbound.ListInput{RepoID: e.RepoID})
			if err != nil {
				return e, err
			}
			selected := contextSelectionAtCode(op.Worktree, e.GitAfter, remoteBranch, all.Snapshots, history)
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
	// A committed callback is authoritative. When it was lost, a new reflog
	// creation after the prepared prefix proves completion; HEAD alone does not.
	if op.Event.Kind == "orphan" {
		return false, nil
	}
	log, err := readBranchLog(op.Worktree, op.Event.Branch)
	if err != nil {
		return false, err
	}
	if op.LogBytes < 0 || op.LogBytes > len(log) || op.LogHash == "" {
		return false, nil
	}
	if domain.HashContent(log[:op.LogBytes]) != op.LogHash {
		return false, nil
	}
	for _, line := range strings.Split(string(log[op.LogBytes:]), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && isZeroGitOID(fields[0]) && fields[1] == op.Event.GitAfter {
			return true, nil
		}
	}
	return false, nil
}
