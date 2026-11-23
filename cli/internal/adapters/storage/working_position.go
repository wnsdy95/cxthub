package storage

import (
	"crypto/sha256"
	"fmt"
)

// NewWorktreeFileStore shares immutable objects and branch refs, but never HEAD.
// The key is Git's worktree admin directory, so moving a worktree keeps its key.
func NewWorktreeFileStore(repoRoot, gitDir, branch, commit string) *FileStore {
	s := NewFileStore(repoRoot)
	if gitDir != "" {
		hash := sha256.Sum256([]byte(gitDir))
		s.worktreeID = fmt.Sprintf("%x", hash[:16])
		s.gitBranch = branch
		s.gitCommit = commit
	}
	return s
}
