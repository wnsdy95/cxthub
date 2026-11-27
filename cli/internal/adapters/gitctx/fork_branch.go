package gitctx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ForkBranches resolves a commit and makes a branch at it, in the repository
// that holds a folder. It changes nothing else: no folder is switched to the
// new branch.
type ForkBranches struct{}

// NewForkBranches creates the adapter.
func NewForkBranches() *ForkBranches { return &ForkBranches{} }

// ResolveCommit returns the full number of the commit a revision names in
// this clone. A revision Git does not hold is an error that says so.
func (*ForkBranches) ResolveCommit(ctx context.Context, cwd, revision string) (string, error) {
	revision = strings.TrimSpace(revision)
	if revision == "" || strings.HasPrefix(revision, "-") {
		return "", fmt.Errorf("%q does not name a commit", revision)
	}
	out, err := gitRead(ctx, cwd, nil, "rev-parse", "--verify", "--quiet", revision+"^{commit}")
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	sha := strings.TrimSpace(string(out))
	if err != nil || !fullCommitNumber(sha) {
		return "", fmt.Errorf("this clone holds no commit %s; fetch it first", revision)
	}
	return sha, nil
}

// CreateBranchAt makes the branch name at commit. A name Git does not take
// and a branch that already exists are errors, and nothing is changed.
func (*ForkBranches) CreateBranchAt(ctx context.Context, cwd, name, commit string) error {
	if name == "" || strings.HasPrefix(name, "-") {
		return fmt.Errorf("%q cannot name a branch", name)
	}
	if !fullCommitNumber(commit) {
		return fmt.Errorf("a branch is made at a full commit number")
	}
	if _, err := gitRead(ctx, cwd, nil, "check-ref-format", "--branch", name); err != nil {
		return fmt.Errorf("%q cannot name a branch", name)
	}
	if _, err := gitRead(ctx, cwd, nil, "show-ref", "--verify", "--quiet", "refs/heads/"+name); err == nil {
		return fmt.Errorf("the branch %s already exists", name)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "branch", "--no-track", "--", name, commit)
	cmd.Env = os.Environ()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("git did not make the branch %s: %s", name, strings.TrimSpace(stderr.String()))
	}
	return nil
}
