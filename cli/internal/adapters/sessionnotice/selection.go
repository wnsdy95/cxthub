// Package sessionnotice reads a worktree selection for identifier-only hook
// delivery. It has no remote client and does not open provider transcripts.
package sessionnotice

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type SelectionReader struct {
	root, gitDir string
	positions    outbound.WorkingPositionReader
}

func NewSelectionReader(root, gitDir string, positions outbound.WorkingPositionReader) *SelectionReader {
	return &SelectionReader{root: root, gitDir: gitDir, positions: positions}
}
func git(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-C", cwd}, args...)...)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_NAMESPACE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
func (s *SelectionReader) ReadNoticeSelection(ctx context.Context, cwd string) (domain.SessionNoticeSelection, error) {
	var none domain.SessionNoticeSelection
	if _, err := providerfs.ReadRepoFile(s.root, ".cxt/HEAD"); err != nil {
		if os.IsNotExist(err) {
			return none, domain.ErrNotFound
		}
		return none, err
	}
	gitDir, err := git(ctx, cwd, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return none, err
	}
	expected, err := filepath.EvalSymlinks(s.gitDir)
	if err != nil {
		return none, err
	}
	actual, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return none, err
	}
	if actual != expected {
		return none, domain.ErrSelectionChanged
	}
	before, err := git(ctx, cwd, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return none, err
	}
	branch, err := git(ctx, cwd, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 1 {
			return none, err
		}
		branch = ""
	}
	// Unconnected replicas cannot construct a cloud MCP repository selection.
	remotes, err := remotecfg.LoadAtRoot(s.root)
	if err != nil {
		return none, err
	}
	origin := remotes["origin"]
	if origin == "" {
		return none, domain.ErrNotFound
	}
	repo := string(remotecfg.RepoIDFor(origin))
	p, err := s.positions.GetWorkingPosition(ctx)
	if err != nil {
		return none, err
	}
	wt := sha256.Sum256([]byte(s.gitDir))
	if p.RepoID != repo || p.WorktreeID != fmt.Sprintf("%x", wt[:16]) || p.GitBranch() != branch || p.GitCommit != before {
		return none, domain.ErrSelectionChanged
	}
	after, err := git(ctx, cwd, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return none, err
	}
	if after != before {
		return none, domain.ErrSelectionChanged
	}
	if p.Snapshot == "" {
		return none, domain.ErrNotFound
	}
	selection := domain.SessionNoticeSelection{RepoID: repo, WorktreeID: p.WorktreeID, BranchID: p.BranchID, Snapshot: p.Snapshot, CodeCommit: after, MemoryPinned: p.MemoryPinned, MemoryHash: p.MemoryHash, MemorySource: p.MemorySource}
	if selection.MemoryHash != "" && selection.MemorySource == "" {
		selection.MemorySource = p.Snapshot
	}
	return selection, selection.Validate()
}
