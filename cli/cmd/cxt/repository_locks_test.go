package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
)

func TestCommandRootLocksUseRequestedReplicaAndWorktree(test *testing.T) {
	ctx := context.Background()
	configured := test.TempDir()
	worktree := test.TempDir()
	other := test.TempDir()
	container := buildContainer(config{RepoRoot: configured}).clictr
	if container.WithSecretsLock == nil {
		test.Fatal("the secrets lock is not composed")
	}
	blocked := errors.New("protected operation failed")
	err := container.WithSecretsLock(ctx, worktree, func() error {
		acquired, err := providerfs.WithCxtLock(ctx, worktree, "secrets", "editing", syscall.LOCK_EX, false, func() error {
			test.Fatal("secrets lock did not exclude another owner")
			return nil
		})
		if err != nil || acquired {
			test.Fatalf("wrong worktree lock: acquired=%t error=%v", acquired, err)
		}
		if err := container.WithSecretsLock(ctx, other, func() error { return nil }); err != nil {
			test.Fatalf("independent worktree blocked: %v", err)
		}
		return blocked
	})
	if !errors.Is(err, blocked) {
		test.Fatalf("protected error lost: %v", err)
	}
	if err := container.WithSecretsLock(ctx, worktree, func() error { return nil }); err != nil {
		test.Fatalf("secrets error leaked lock ownership: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configured, ".cxt")); !errors.Is(err, os.ErrNotExist) {
		test.Fatalf("secrets lock used configured/shared root: %v", err)
	}
}
