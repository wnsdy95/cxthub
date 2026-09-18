package capture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// RegisteredSession requires an exact, validated hook registration in this
// worktree. Background observation never discovers a newer sibling session.
func RegisteredSession(cwd string, provider domain.ProviderKind, id string) (AppSession, bool) {
	for _, s := range ActiveAppSessions(cwd) {
		if s.Provider == provider && s.SessionID == id {
			return s, true
		}
	}
	return AppSession{}, false
}

// WatchSession owns an OS-released lock, so overlapping lifecycle hooks cannot
// create duplicate observers and a crash cannot leave a stale ownership lock.
// poll starts a fresh command: worktree Git position must be resolved again on
// every capture, never retained from the observer's startup.
func WatchSession(ctx context.Context, cwd string, provider domain.ProviderKind, id string, poll func(context.Context) error) error {
	root, _, enabled := appSessionRoots(ctx, cwd)
	if !enabled || !validHookSessionID(id) {
		return nil
	}
	if _, ok := RegisteredSession(cwd, provider, id); !ok {
		return nil
	}
	path, err := providerfs.PrepareRepoFile(root, filepath.Join(".cxt", "capture", captureStateBase(ctx, provider, cwd, id)+".observer"), 0700)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return observeSession(ctx, cwd, provider, id, 10*time.Second, 30*time.Minute, poll)
}

func observeSession(ctx context.Context, cwd string, provider domain.ProviderKind, id string, interval, idle time.Duration, poll func(context.Context) error) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var size int64 = -1
	var modified time.Time
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s, ok := RegisteredSession(cwd, provider, id)
		if !ok {
			return nil
		}
		info, err := os.Stat(s.Path)
		if err != nil || time.Since(info.ModTime()) > idle {
			return nil
		}
		if info.Size() != size || !info.ModTime().Equal(modified) {
			// Retain the old observation on failure, retrying even without new text.
			if err := poll(ctx); err == nil {
				size, modified = info.Size(), info.ModTime()
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
