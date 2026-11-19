package gitctx

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// CollectorFiles keeps the automatic collector's local files beside Git's own
// files of the repository, shared by all its worktrees and never in a working
// tree: what was last sent, the lock one running collector holds, and the
// request for one more run.
type CollectorFiles struct{}

// NewCollectorFiles creates the adapter.
func NewCollectorFiles() *CollectorFiles { return &CollectorFiles{} }

const (
	collectorStateFile   = "collector-v1.json"
	collectorLockFile    = "collector.lock"
	collectorRequestFile = "collector.again"
)

func collectorDir(ctx context.Context, cwd string) (string, error) {
	common, err := gitCommonDir(ctx, cwd)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(common, "cxt")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// LoadCollectorState returns what the collector remembers. A state that is
// missing, unreadable or of another layout is an empty one: it is a memory of
// what was sent, so losing it only makes the next run send more.
func (*CollectorFiles) LoadCollectorState(ctx context.Context, cwd string) (domain.CollectorState, error) {
	dir, err := collectorDir(ctx, cwd)
	if err != nil {
		return domain.CollectorState{}, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, collectorStateFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.CollectorState{}, nil
		}
		return domain.CollectorState{}, err
	}
	var state domain.CollectorState
	if json.Unmarshal(raw, &state) != nil || state.Version != domain.CollectorStateVersion {
		return domain.CollectorState{}, nil
	}
	return state, nil
}

// SaveCollectorState replaces the state as a whole.
func (*CollectorFiles) SaveCollectorState(ctx context.Context, cwd string, state domain.CollectorState) error {
	dir, err := collectorDir(ctx, cwd)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+collectorStateFile+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), filepath.Join(dir, collectorStateFile))
}

// WithCollectorLock runs use as the repository's one running collector and
// returns true. When another collector is already running it leaves a request
// for one more run instead and returns false: whatever woke this one will be
// read by that one.
//
// use receives another, which answers whether a request arrived since the
// run began and takes it.
func (*CollectorFiles) WithCollectorLock(ctx context.Context, cwd string, use func(another func() bool) error) (bool, error) {
	dir, err := collectorDir(ctx, cwd)
	if err != nil {
		return false, err
	}
	request := filepath.Join(dir, collectorRequestFile)
	lock, err := os.OpenFile(filepath.Join(dir, collectorLockFile), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return false, err
		}
		// The request is written while the other collector may be finishing.
		// It takes requests only while it holds the lock, so one written just
		// after its last look stays for the next collector to find.
		if file, err := os.OpenFile(request, os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600); err == nil {
			file.Close()
		}
		return false, nil
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	another := func() bool { return os.Remove(request) == nil }
	// A request left before this collector started is answered by its first run.
	another()
	return true, use(another)
}

// RequestRunIfBusy reports whether a collector is running in the repository,
// and when one is, leaves it the request for one more run. A hook asks this
// before it starts a collector of its own, so that a busy repository costs a
// hook one small file and no new process.
func (*CollectorFiles) RequestRunIfBusy(ctx context.Context, cwd string) bool {
	dir, err := collectorDir(ctx, cwd)
	if err != nil {
		return false
	}
	lock, err := os.OpenFile(filepath.Join(dir, collectorLockFile), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		return false
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return false
	}
	if file, err := os.OpenFile(filepath.Join(dir, collectorRequestFile), os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600); err == nil {
		file.Close()
	}
	return true
}
