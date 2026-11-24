package gitctx

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// BranchIDs reads and stores branch IDs in Git's own per-branch configuration
// (branch.<name>.cxtBranchId). Git keeps that setting with the branch: a
// rename moves it, deleting the branch drops it, and every worktree of the
// repository sees the same value.
type BranchIDs struct{}

// NewBranchIDs creates the adapter.
func NewBranchIDs() *BranchIDs { return &BranchIDs{} }

// LocalBranches lists the repository's branches with the ID and issued-for
// name their configuration holds. It reads only.
func (*BranchIDs) LocalBranches(ctx context.Context, cwd string) ([]domain.LocalBranch, error) {
	// One listing gives the local branches with their configured upstream and
	// the remote-tracking branches of origin.
	branches, tracked := followedBranches(ctx, cwd)
	refs, err := gitRead(ctx, cwd, nil, "for-each-ref", "--format=%(refname)%00%(upstream)", "refs/heads", branches)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	const local = "refs/heads/"
	byName := map[string]*domain.LocalBranch{}
	onOrigin := map[string]struct{}{}
	var names []string
	for _, line := range strings.Split(string(refs), "\n") {
		ref, upstream, _ := strings.Cut(strings.TrimRight(line, "\r"), "\x00")
		switch {
		case strings.HasPrefix(ref, tracked):
			if name := strings.TrimPrefix(ref, tracked); name != "HEAD" {
				onOrigin[name] = struct{}{}
			}
		case strings.HasPrefix(ref, local):
			name := strings.TrimPrefix(ref, local)
			byName[name] = &domain.LocalBranch{Name: name, Remote: strings.TrimPrefix(upstream, tracked)}
			if !strings.HasPrefix(upstream, tracked) {
				byName[name].Remote = ""
			}
			names = append(names, name)
		}
	}
	for _, name := range names {
		branch := byName[name]
		if _, ok := onOrigin[branch.Remote]; branch.Remote != "" && !ok {
			// The configured upstream is not among origin's branches Git knows.
			branch.Remote = ""
		}
		if branch.Remote == "" {
			if _, ok := onOrigin[name]; ok {
				branch.Remote = name
			}
		}
	}
	// Git prints a setting as its key, a newline, its value and a NUL. Section
	// and variable names come back in lower case; the branch name keeps its own.
	pattern := `^branch\..*\.(` + strings.ToLower(domain.BranchIDConfigKey) + `|` + strings.ToLower(domain.BranchIDIssuedForConfigKey) + `)$`
	settings, err := gitRead(ctx, cwd, nil, "config", "--local", "-z", "--get-regexp", pattern)
	if err != nil {
		// Exit status 1 means no setting matched; anything else is a failure.
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("read branch settings: %w", err)
		}
	}
	for _, entry := range bytes.Split(settings, []byte{0}) {
		key, value, ok := strings.Cut(string(entry), "\n")
		if !ok {
			continue
		}
		rest := strings.TrimPrefix(key, "branch.")
		dot := strings.LastIndex(rest, ".")
		if dot < 0 {
			continue
		}
		branch := byName[rest[:dot]]
		if branch == nil {
			continue // a setting left for a branch that no longer exists
		}
		switch rest[dot+1:] {
		case strings.ToLower(domain.BranchIDConfigKey):
			branch.ID = value
		case strings.ToLower(domain.BranchIDIssuedForConfigKey):
			branch.IssuedFor = value
		}
	}
	sort.Strings(names)
	out := make([]domain.LocalBranch, 0, len(names))
	for _, name := range names {
		out = append(out, *byName[name])
	}
	return out, nil
}

// StoreBranchID writes an assignment into the branch's configuration. The
// issued-for name is written first: a write interrupted between the two
// leaves a branch without an ID, which the next run fills.
func (*BranchIDs) StoreBranchID(ctx context.Context, cwd string, assignment domain.BranchIDAssignment) error {
	if assignment.Name == "" || !domain.ValidBranchID(assignment.ID) {
		return fmt.Errorf("invalid branch ID assignment")
	}
	for _, setting := range [][2]string{{domain.BranchIDIssuedForConfigKey, assignment.Name}, {domain.BranchIDConfigKey, assignment.ID}} {
		cmd := exec.CommandContext(ctx, "git", "-C", cwd, "config", "--local", "branch."+assignment.Name+"."+setting[0], setting[1])
		cmd.Env = os.Environ()
		if out, err := cmd.CombinedOutput(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("store branch ID: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// WithBranchIDLock runs use while no other cxt process reads and assigns
// branch IDs in the same repository. Two processes that both found a branch
// without an ID would otherwise each store one.
func (*BranchIDs) WithBranchIDLock(ctx context.Context, cwd string, use func() error) error {
	common, err := gitCommonDir(ctx, cwd)
	if err != nil {
		return err
	}
	dir := filepath.Join(common, "cxt")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "branch-ids.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return use()
}

// NewBranchID returns a random ID.
func NewBranchID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
