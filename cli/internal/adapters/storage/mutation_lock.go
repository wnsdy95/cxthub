package storage

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"syscall"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

// An OS lock cannot expire while its owner is still writing, and is released
// by the kernel on process exit. Keep the inode permanently: unlinking a lock
// file would allow old and new open descriptors to protect different inodes.
func (s *FileStore) withMutationLock(ctx context.Context, namespace, key string, fn func() error) error {
	_, err := s.withOSLock(ctx, namespace, key, syscall.LOCK_EX, true, func() error {
		return s.withLegacyMutationLock(ctx, namespace, key, fn)
	})
	return err
}

func (s *FileStore) withOSLock(ctx context.Context, namespace, key string, mode int, wait bool, fn func() error) (bool, error) {
	return providerfs.WithCxtLock(ctx, s.repoRoot, namespace, key, mode, wait, fn)
}

func deadMutationOwner(path string) bool {
	raw, err := readCxtFile(path)
	if err != nil {
		return false
	}
	pidText, _, ok := strings.Cut(string(raw), "-")
	if !ok {
		return false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return false
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func liveMutationOwner(path string) bool {
	raw, err := readCxtFile(path)
	if err != nil {
		return false
	}
	pidText, _, ok := strings.Cut(string(raw), "-")
	if !ok {
		return false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return false
	}
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// WithSecretsLock serializes plaintext and edit-baseline changes in one worktree.
func (s *FileStore) WithSecretsLock(ctx context.Context, fn func() error) error {
	return s.withMutationLock(ctx, "secrets", "editing", fn)
}
