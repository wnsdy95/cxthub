package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
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
	dir := filepath.Join(s.storeDir(), "locks", namespace)
	if err := validateCxtDir(dir); err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return false, err
	}
	path := filepath.Join(dir, key+".flock")
	if err := validateCxtWritePath(path); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, domain.ErrHashMismatch
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		err = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return false, err
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !wait {
			return false, nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true, fn()
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
