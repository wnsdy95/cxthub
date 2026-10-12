package capture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
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

type SessionWatch struct {
	Poll    func(context.Context) error
	Publish func(context.Context) error
	Wake    func(context.Context)
}

// WatchSession owns an OS-released lock, so overlapping lifecycle hooks cannot
// create duplicate observers and a crash cannot leave a stale ownership lock.
// poll starts a fresh command: worktree Git position must be resolved again on
// every capture, never retained from the observer's startup.
func WatchSession(ctx context.Context, cwd string, provider domain.ProviderKind, id string, watch SessionWatch) error {
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
	return observeSession(ctx, cwd, provider, id, 10*time.Second, 30*time.Minute, watch)
}

func observeSession(ctx context.Context, cwd string, provider domain.ProviderKind, id string, interval, idle time.Duration, watch SessionWatch) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var size int64 = -1
	var modified time.Time
	var policy string
	root, _, _ := appSessionRoots(ctx, cwd)
	pending := false
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
		if watch.Wake != nil {
			watch.Wake(ctx)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		currentPolicy, policyErr := ScrubPolicyFingerprint(root)
		if !pending && policyErr == nil && (info.Size() != size || !info.ModTime().Equal(modified) || policy != currentPolicy) {
			// Retain the old observation on failure, retrying even without new text.
			if err := watch.Poll(ctx); err == nil {
				size, modified, policy = info.Size(), info.ModTime(), currentPolicy
				pending = watch.Publish != nil
			}
		}
		// Finish the durable capture before reading a growing transcript again.
		// A failed upload cannot starve behind repeated expensive recaptures.
		if pending && watch.Publish(ctx) == nil {
			pending = false
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
