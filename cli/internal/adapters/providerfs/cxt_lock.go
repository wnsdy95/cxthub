package providerfs

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// WithCxtLock is the existing permanent-inode flock protocol shared by config
// and storage. Never unlink a lock file or hold this lock over network work.
func WithCxtLock(ctx context.Context, repoRoot, namespace, key string, mode int, wait bool, fn func() error) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	dir := filepath.Join(repoRoot, ".cxt", "locks", namespace)
	if err := ValidateCxtDir(dir); err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return false, err
	}
	path := filepath.Join(dir, key+".flock")
	if err := ValidateCxtWritePath(path); err != nil {
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

func ValidateCxtDir(dir string) error {
	current := filepath.Clean(dir)
	var chain []string
	foundRoot := false
	for {
		chain = append(chain, current)
		if filepath.Base(current) == ".cxt" {
			foundRoot = true
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	if !foundRoot {
		return domain.ErrHashMismatch
	}
	for i := len(chain) - 1; i >= 0; i-- {
		info, err := os.Lstat(chain[i])
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return domain.ErrHashMismatch
		}
	}
	return nil
}

func ValidateCxtWritePath(path string) error {
	if err := ValidateCxtDir(filepath.Dir(path)); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return domain.ErrHashMismatch
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
