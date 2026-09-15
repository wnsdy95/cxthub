package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
)

// Confirmation is explicit evidence supplied by the operator, never presented
// as a recovered Git callback. It is usable only while the exact recorded
// worktree still has that unborn HEAD. Later commits require stronger evidence.
func confirmOrphanRecovery(ctx context.Context, c *Container, cwd, id string) error {
	j, err := branchjournal.Open(ctx, cwd)
	if err != nil {
		return err
	}
	repo, err := remotecfg.Wrap(cwd, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
	if err != nil {
		return err
	}
	return j.Transaction(ctx, func() error {
		if err := j.Bind(repo.ID); err != nil {
			return err
		}
		ops, err := j.List()
		if err != nil {
			return err
		}
		for _, op := range ops {
			if op.Event.ID != id {
				continue
			}
			if op.Event.RepoID != repo.ID || op.Event.Kind != "orphan" {
				return fmt.Errorf("operation is not an orphan creation in this repository")
			}
			if op.Event.RecoveryEvidence == "user-confirmed-unborn-head" && (op.Phase == "committed" || op.Phase == "applied") {
				return nil
			}
			if op.Phase != "prepared" || op.Resolved {
				return fmt.Errorf("only an unresolved prepared orphan operation can be confirmed")
			}
			root := gitOut(cwd, "rev-parse", "--show-toplevel")
			expected, err := filepath.EvalSymlinks(op.Worktree)
			if err != nil {
				return err
			}
			actual, err := filepath.EvalSymlinks(root)
			if err != nil {
				return err
			}
			if expected != actual {
				return fmt.Errorf("confirmation must run in the recorded worktree %s", op.Worktree)
			}
			headPath := gitOut(cwd, "rev-parse", "--path-format=absolute", "--git-path", "HEAD")
			if !filepath.IsAbs(headPath) {
				return fmt.Errorf("cannot locate Git HEAD")
			}
			// Use Git's own HEAD lock to exclude concurrent checkout/commit publication.
			lock, err := os.OpenFile(headPath+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
			if err != nil {
				return fmt.Errorf("Git HEAD is busy: %w", err)
			}
			defer func() { lock.Close(); os.Remove(headPath + ".lock") }()
			refPath := gitOut(cwd, "rev-parse", "--path-format=absolute", "--git-path", op.GitRef)
			common := gitOut(cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
			rel, err := filepath.Rel(common, refPath+".lock")
			if err != nil || strings.HasPrefix(rel, "..") {
				return fmt.Errorf("unsafe Git reference path")
			}
			safe, err := providerfs.PrepareRepoFile(common, rel, 0700)
			if err != nil {
				return err
			}
			refLock, err := os.OpenFile(safe, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
			if err != nil {
				return fmt.Errorf("Git branch is busy: %w", err)
			}
			defer func() { refLock.Close(); os.Remove(safe) }()
			raw, err := os.ReadFile(headPath)
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(raw)) != "ref: "+op.GitRef {
				return fmt.Errorf("current HEAD does not match the prepared orphan; no record changed")
			}
			cmd := exec.CommandContext(ctx, "git", "-C", cwd, "show-ref", "--verify", "--quiet", op.GitRef)
			err = cmd.Run()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 {
				return fmt.Errorf("the recorded branch is not verifiably unborn; no record changed")
			}
			if c.History == nil {
				return fmt.Errorf("context history service unavailable")
			}
			if _, err := c.History.ValidateHistorySource(ctx, op.Event); err != nil {
				return err
			}
			op.Event.RecoveryEvidence = "user-confirmed-unborn-head"
			op.Phase = "committed"
			op.LastError = ""
			return j.Save(op)
		}
		return fmt.Errorf("unknown branch operation %q", id)
	})
}
