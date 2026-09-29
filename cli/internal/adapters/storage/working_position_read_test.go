package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestReadWorkingPositionDoesNotReplayOrCreateLocks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
	repo := string(domain.HashContent([]byte("pure worktree selection")))
	position := domain.WorkingPosition{RepoID: repo, WorktreeID: store.worktreeID, Branch: "main", BranchID: "logical-main", GitCommit: store.gitCommit}
	if err := store.PutWorkingPosition(ctx, position); err != nil {
		t.Fatal(err)
	}
	// Simulate a corrupt interrupted writer. Queries must not replay it, and
	// must not need a writable locks directory merely to read an atomic file.
	if err := os.WriteFile(store.workingCommitPath(), []byte("broken journal"), 0600); err != nil {
		t.Fatal(err)
	}
	locks := filepath.Join(store.storeDir(), "locks")
	if err := os.RemoveAll(locks); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadWorkingPosition(ctx, repo)
	if err != nil || got.BranchID != position.BranchID || got.GitCommit != position.GitCommit {
		t.Fatalf("read=%+v %v", got, err)
	}
	if _, err := os.Stat(locks); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("query created locks: %v", err)
	}
	raw, err := os.ReadFile(store.workingCommitPath())
	if err != nil || string(raw) != "broken journal" {
		t.Fatalf("query touched journal: %s %v", raw, err)
	}
	if _, err := store.ReadWorkingPosition(ctx, string(domain.HashContent([]byte("other repo")))); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("repository confusion: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.ReadWorkingPosition(canceled, repo); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}
