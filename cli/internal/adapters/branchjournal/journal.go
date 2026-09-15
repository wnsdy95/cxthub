// Package branchjournal records Git operations outside the disposable .cxt
// replica. Atomic writes and directory fsync precede Git's prepared vote.
package branchjournal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type Operation struct {
	Event     domain.HistoryEvent `json:"event"`
	Phase     string              `json:"phase"`
	GitRef    string              `json:"git_ref"`
	Worktree  string              `json:"worktree"`
	LastError string              `json:"last_error,omitempty"`
	Resolved  bool                `json:"resolved,omitempty"`
	GitPID    string              `json:"git_pid,omitempty"`
	LogBytes  int                 `json:"log_bytes,omitempty"`
	LogHash   domain.ContentHash  `json:"log_hash,omitempty"`
}

type Journal struct{ gitDir string }

func Open(ctx context.Context, cwd string) (*Journal, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(string(out))
	if !filepath.IsAbs(root) {
		root = filepath.Join(cwd, root)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return &Journal{gitDir: root}, nil
}

func (j *Journal) Enabled() bool {
	_, err := providerfs.ReadRepoFile(j.gitDir, "cxt/enabled")
	return err == nil
}

func (j *Journal) Status() (bool, error) {
	raw, err := providerfs.ReadRepoFile(j.gitDir, "cxt/enabled")
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(raw) != "1\n" {
		return false, fmt.Errorf("corrupt CXTHub registration in Git directory")
	}
	return true, nil
}

func (j *Journal) Enable() error { return j.write("cxt/enabled", []byte("1\n")) }

func (j *Journal) Bind(repoID string) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	raw, err := providerfs.ReadRepoFile(j.gitDir, "cxt/repository")
	if err == nil {
		if strings.TrimSpace(string(raw)) != repoID {
			return fmt.Errorf("repository identity conflicts with durable Git registration")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return j.write("cxt/repository", []byte(repoID+"\n"))
}

func (j *Journal) write(path string, raw []byte) error {
	if err := providerfs.WriteRepoFileAtomic(j.gitDir, path, raw, 0o600); err != nil {
		return err
	}
	// Also persist directory entries, including directories created on first use.
	for dir := filepath.Dir(filepath.Join(j.gitDir, path)); ; dir = filepath.Dir(dir) {
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = f.Sync()
		_ = f.Close()
		if err != nil {
			return err
		}
		if dir == j.gitDir {
			break
		}
	}
	return nil
}

func (j *Journal) Transaction(ctx context.Context, fn func() error) error {
	path, err := providerfs.PrepareRepoFile(j.gitDir, "cxt/journal.lock", 0o700)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (j *Journal) List() ([]Operation, error) {
	// Validate the directory even when it is empty; a symlink must not turn
	// an unsafe journal into a seemingly empty queue.
	if info, err := os.Lstat(filepath.Join(j.gitDir, "cxt", "operations")); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe branch journal directory")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(j.gitDir, "cxt", "operations"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]Operation, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		raw, err := providerfs.ReadRepoFile(j.gitDir, filepath.Join("cxt", "operations", entry.Name()))
		if err != nil {
			return nil, err
		}
		var op Operation
		if err = json.Unmarshal(raw, &op); err != nil {
			return nil, fmt.Errorf("corrupt branch journal %s: %w", entry.Name(), err)
		}
		if err = domain.ValidateHistoryEvent(op.Event); err != nil {
			return nil, err
		}
		if entry.Name() != op.Event.ID+".json" {
			return nil, fmt.Errorf("branch journal identity mismatch")
		}
		switch op.Phase {
		case "prepared", "committed", "applied", "aborted":
		default:
			return nil, fmt.Errorf("invalid branch journal phase")
		}
		out = append(out, op)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Event.CreatedAt.Before(out[k].Event.CreatedAt) })
	return out, nil
}

func NewID() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}

// Save is called while Transaction is held. Completed records are retained for
// audit; replay changes only phase/error, never the prepared source or ID.
func (j *Journal) Save(op Operation) error {
	if err := domain.ValidateHistoryEvent(op.Event); err != nil {
		return err
	}
	raw, err := json.Marshal(op)
	if err != nil {
		return err
	}
	return j.write(filepath.Join("cxt", "operations", op.Event.ID+".json"), raw)
}

// Repository returns the durable Git-side binding without creating or repairing it.
func (j *Journal) Repository() (string, error) {
	raw, err := providerfs.ReadRepoFile(j.gitDir, "cxt/repository")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(raw))
	if err := domain.ValidateContentHash(domain.ContentHash(id)); err != nil {
		return "", err
	}
	return id, nil
}

// StartRepair persists the recovery identity outside the damaged replica.
func (j *Journal) StartRepair(repoID string) (string, error) {
	if err := j.Bind(repoID); err != nil {
		return "", err
	}
	id, err := NewID()
	if err != nil {
		return "", err
	}
	path := filepath.Join("cxt", "repairs", id, "repository")
	if err := j.write(path, []byte(repoID+"\n")); err != nil {
		return "", err
	}
	return filepath.Dir(filepath.Join(j.gitDir, path)), nil
}
