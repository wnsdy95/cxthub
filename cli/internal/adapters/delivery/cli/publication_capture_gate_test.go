package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type captureGatePositionHistory struct {
	*app.ContextHistoryService
	afterRead func(domain.WorkingPosition) error
}

func (h captureGatePositionHistory) CurrentPosition(ctx context.Context) (domain.WorkingPosition, error) {
	p, err := h.ContextHistoryService.CurrentPosition(ctx)
	if err == nil && h.afterRead != nil {
		err = h.afterRead(p)
	}
	return p, err
}

// Embedding the old, narrow port deliberately hides both optional capabilities.
type captureGateLegacyHistory struct{ inbound.ContextHistory }

func captureGateFixture(t *testing.T) (string, *Container, *storage.FileStore, domain.Repo) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	cwd := t.TempDir()
	runLifecycleGit(t, cwd, "init", "-q", "-b", "main")
	runLifecycleGit(t, cwd, "config", "core.hooksPath", "/dev/null")
	runLifecycleGit(t, cwd, "config", "user.name", "test")
	runLifecycleGit(t, cwd, "config", "user.email", "test@example.test")
	runLifecycleGit(t, cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "initial")
	git := gitctx.NewGitContextAdapter()
	repo, err := git.CurrentRepo(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	st := storage.NewWorktreeFileStore(cwd, gitOut(cwd, "rev-parse", "--absolute-git-dir"), "main", gitOut(cwd, "rev-parse", "HEAD"))
	if err := st.PutWorkingPosition(context.Background(), domain.WorkingPosition{RepoID: repo.ID, Branch: "main", GitCommit: gitOut(cwd, "rev-parse", "HEAD")}); err != nil {
		t.Fatal(err)
	}
	return cwd, &Container{History: app.NewContextHistoryService(st, st), ResolveRepo: git.CurrentRepo}, st, repo
}

// Probe the very same flock used by setup/admission, without a timeout or sleep.
// A successful probe is released immediately so the real competing admission
// can reproduce the old interleaving before CurrentPosition returns.
func captureGateExclusiveAvailable(cwd string) (bool, error) {
	dir := filepath.Join(cwd, ".cxt", "locks", "first-tracking")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "repo.flock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return false, err
	}
	return true, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func TestBeginCommitCaptureGateKeepsAdmissionOutsideFrozenIntent(t *testing.T) {
	cwd, c, st, repo := captureGateFixture(t)
	otherRepo := string(domain.HashContent([]byte("competing connected repository")))
	peer := storage.NewWorktreeFileStore(cwd, gitOut(cwd, "rev-parse", "--absolute-git-dir"), "main", gitOut(cwd, "rev-parse", "HEAD"))
	checked, slipped := false, false
	c.History = captureGatePositionHistory{ContextHistoryService: c.History.(*app.ContextHistoryService), afterRead: func(domain.WorkingPosition) error {
		checked = true
		available, err := captureGateExclusiveAvailable(cwd)
		if err != nil || !available {
			return err
		}
		// The old implementation allows a real admission after its position
		// read and before writing the intent. The new SH gate excludes this.
		if err := peer.EnsureCapturePosition(context.Background(), otherRepo); err != nil {
			return err
		}
		slipped = true
		return nil
	}}
	pass, err := beginCommitCapture(context.Background(), c, cwd, []string{domain.ProviderClaude})
	if err != nil || pass == nil {
		t.Fatalf("capture: %+v %v", pass, err)
	}
	if !checked {
		t.Fatal("position-read boundary was not exercised")
	}
	position, err := st.GetWorkingPosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if slipped || position.RepoID != repo.ID || pass.Proof.RepoID != position.RepoID {
		t.Fatalf("competing admission slipped between read and intent: slipped=%v position=%s intent=%s", slipped, position.RepoID, pass.Proof.RepoID)
	}
	if _, err := os.Stat(filepath.Join(cwd, pass.relativePath())); err != nil {
		t.Fatal("successful capture has no durable intent", err)
	}
	available, err := captureGateExclusiveAvailable(cwd)
	if err != nil || !available {
		t.Fatalf("capture gate leaked: available=%v err=%v", available, err)
	}
	// Once the gate releases, the complete intent itself must reject bootstrap.
	if err := peer.EnsureCapturePosition(context.Background(), otherRepo); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatalf("durable intent did not block later normalization: %v", err)
	}
}

func TestBeginCommitCaptureGateRechecksAndReleasesOnFailure(t *testing.T) {
	for _, reason := range []string{"connection-changed", "position-failed"} {
		t.Run(reason, func(t *testing.T) {
			cwd, c, _, repo := captureGateFixture(t)
			want := domain.ErrSelectionChanged
			if reason == "connection-changed" {
				calls := 0
				c.ResolveRepo = func(context.Context, string) (domain.Repo, error) {
					calls++
					current := repo
					if calls > 1 {
						available, err := captureGateExclusiveAvailable(cwd)
						if err != nil || available {
							t.Fatalf("connection recheck was not gated: %v %v", available, err)
						}
						current.ID = string(domain.HashContent([]byte("new connection")))
					}
					return current, nil
				}
			} else {
				want = errors.New("position read failed")
				c.History = captureGatePositionHistory{ContextHistoryService: c.History.(*app.ContextHistoryService), afterRead: func(domain.WorkingPosition) error { return want }}
			}
			pass, err := beginCommitCapture(context.Background(), c, cwd, []string{domain.ProviderClaude})
			if !errors.Is(err, want) || pass != nil {
				t.Fatalf("failed boundary persisted a pass: %+v %v", pass, err)
			}
			intents, err := filepath.Glob(filepath.Join(cwd, ".cxt", "worktrees", "*", "capture-passes", "*.json"))
			if err != nil || len(intents) != 0 {
				t.Fatalf("failed capture left an intent: %v %v", intents, err)
			}
			available, err := captureGateExclusiveAvailable(cwd)
			if err != nil || !available {
				t.Fatalf("failed capture leaked gate: %v %v", available, err)
			}
		})
	}
}

func TestBeginCommitCaptureGateLegacyHistoryCompatibility(t *testing.T) {
	cwd, c, _, repo := captureGateFixture(t)
	c.History = captureGateLegacyHistory{c.History}
	c.ResolveRepo = func(context.Context, string) (domain.Repo, error) {
		t.Fatal("legacy History unexpectedly required optional admission")
		return domain.Repo{}, nil
	}
	pass, err := beginCommitCapture(context.Background(), c, cwd, []string{domain.ProviderClaude})
	if err != nil || pass == nil || pass.Proof.RepoID != repo.ID {
		t.Fatalf("legacy History capture: %+v %v", pass, err)
	}
}
