package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type setupConnection struct {
	*tracking290Sync
	repo      domain.Repo
	after     func()
	failAfter error
}

func (s *setupConnection) Connect(context.Context, inbound.SyncInput) (inbound.ConnectOutput, error) {
	return inbound.ConnectOutput{Repo: s.repo}, nil
}

// Only registration/remote transport are fake. Preparation, fetch verification,
// application, real FileStore and Git upstream reads use production code.
func setupTrackingFixture(t *testing.T) *tracking290Fixture {
	t.Helper()
	f := newTracking290Fixture(t, false)
	// Remove only this test's synthetic baseline; a fresh clone has no local context.
	if err := os.RemoveAll(filepath.Join(f.cwd, ".cxt")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CXT_TOKEN", "")
	if err := remotecfg.Save(f.cwd, remotecfg.Remotes{"origin": "https://example.invalid/team/context"}); err != nil {
		t.Fatal(err)
	}
	runLifecycleGit(t, f.cwd, "remote", "add", "origin", "https://example.invalid/team/code.git")
	runLifecycleGit(t, f.cwd, "update-ref", "refs/remotes/origin/team-task", f.oid)
	runLifecycleGit(t, f.cwd, "config", "branch.local-task.remote", "origin")
	runLifecycleGit(t, f.cwd, "config", "branch.local-task.merge", "refs/heads/team-task")
	gc := remotecfg.Wrap(f.cwd, gitctx.NewGitContextAdapter())
	repo, err := gc.CurrentRepo(context.Background(), f.cwd)
	if err != nil {
		t.Fatal(err)
	}
	f.repo = repo.ID
	f.remote.ref.RepoID = repo.ID
	for i := range f.remote.snaps {
		f.remote.snaps[i].RepoID = repo.ID
	}
	for i := range f.remote.history {
		f.remote.history[i].RepoID = repo.ID
	}
	f.c.Init = app.NewInitRepoService(gc, f.store)
	f.c.Sync = &setupConnection{tracking290Sync: f.sync, repo: repo}
	return f
}

func TestSetupFirstTrackingUsesObservedCodePin(t *testing.T) {
	f := setupTrackingFixture(t)
	if err := runSetup(context.Background(), f.c, f.cwd, []string{"--no-login"}); err != nil {
		t.Fatal(err)
	}
	p, err := f.store.GetWorkingPosition(context.Background())
	if err != nil || p.BranchID != "remote-task-R" || p.Snapshot != f.a || p.SharedTarget != f.b || p.MemoryHash != f.m1 || p.GitBranch() != "local-task" || p.GitCommit != f.oid {
		t.Fatalf("first setup did not select proven R/A/M1 at code: %+v %v", p, err)
	}
	if f.remote.pulls != 1 {
		t.Fatalf("fetches=%d", f.remote.pulls)
	}
}

func TestSetupRepeatPreservesNewerPositionWithoutFetch(t *testing.T) {
	f := setupTrackingFixture(t)
	ctx := context.Background()
	if err := runSetup(ctx, f.c, f.cwd, []string{"--no-login"}); err != nil {
		t.Fatal(err)
	}
	p, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Snapshot, p.SharedTarget, p.MemoryHash, p.MemorySource, p.Rewound, p.Selection = f.b, f.b, "", "", false, nil
	if err := f.store.PutWorkingPosition(ctx, p); err != nil {
		t.Fatal(err)
	}
	before, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := runSetup(ctx, f.c, f.cwd, []string{"--no-login"}); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(before, after) || f.remote.pulls != 1 {
		t.Fatalf("setup refreshed existing position/fetched: equal=%v pulls=%d err=%v", reflect.DeepEqual(before, after), f.remote.pulls, err)
	}
}

func TestSetupFirstTrackingStopsBeforeHooks(t *testing.T) {
	for _, name := range []string{"missing-proof", "wrong-origin", "Git-moves-during-fetch", "connection-changes-after-observation", "capture-during-fetch", "empty-position-changes-during-fetch", "foreign-snapshot-during-fetch", "stage-during-fetch"} {
		t.Run(name, func(t *testing.T) {
			f := setupTrackingFixture(t)
			conn := f.c.Sync.(*setupConnection)
			switch name {
			case "missing-proof":
				f.remote.history = nil
			case "wrong-origin":
				conn.repo.GitRemoteURL = "https://example.invalid/other/repo"
			case "Git-moves-during-fetch":
				conn.after = func() { runLifecycleGit(t, f.cwd, "config", "branch.local-task.merge", "refs/heads/other") }
			case "connection-changes-after-observation":
				conn.after = func() {
					if err := remotecfg.Save(f.cwd, remotecfg.Remotes{"origin": "https://example.invalid/other/context"}); err != nil {
						t.Fatal(err)
					}
				}
			case "capture-during-fetch":
				conn.after = func() {
					if err := f.store.PutPending(context.Background(), domain.Pending{SessionID: "synthetic", RepoID: string(domain.HashContent([]byte("provisional"))), Branch: "local-task", Target: f.a}); err != nil {
						t.Fatal(err)
					}
				}
			case "empty-position-changes-during-fetch":
				conn.after = func() {
					p, err := f.store.GetWorkingPosition(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					p.Branch = "other"
					if err = f.store.PutWorkingPosition(context.Background(), p); err != nil {
						t.Fatal(err)
					}
				}
			case "foreign-snapshot-during-fetch":
				conn.after = func() {
					id, err := f.store.PutDoc(context.Background(), domain.SessionDoc{CIR: domain.CIRDocument{}})
					if err != nil {
						t.Fatal(err)
					}
					if err = f.store.PutSnapshot(context.Background(), domain.Snapshot{ID: id, DocHash: id, RepoID: string(domain.HashContent([]byte("provisional"))), Branch: "local-task"}); err != nil {
						t.Fatal(err)
					}
				}
			case "stage-during-fetch":
				conn.after = func() {
					idx, p, err := f.store.ReadStaging(context.Background(), f.repo)
					if err != nil {
						t.Fatal(err)
					}
					next := idx
					next.Sequence++
					next = next.WithRevision()
					if err = f.store.CompareAndSwapStaging(context.Background(), idx.Revision, next, p); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := runSetup(context.Background(), f.c, f.cwd, []string{"--no-login"})
			if err == nil || !strings.Contains(err.Error(), "setup tracking stopped") {
				t.Fatalf("want actionable setup failure, got %v", err)
			}
			if name == "connection-changes-after-observation" {
				if !errors.Is(err, domain.ErrSelectionChanged) || f.remote.pulls != 1 || f.sync.err != nil {
					t.Fatalf("expected connection rejection after successful observation: err=%v pulls=%d observedErr=%v", err, f.remote.pulls, f.sync.err)
				}
				origin, _ := remotecfg.Origin(f.cwd)
				p, perr := f.store.GetWorkingPosition(context.Background())
				events, herr := f.store.ListHistoryEvents(context.Background(), f.repo)
				_, rerr := f.store.GetRef(context.Background(), f.repo, domain.RefBranch, "team-task")
				if origin != "https://example.invalid/other/context" || perr != nil || p.RepoID != f.repo || p.Snapshot != "" || p.Selection != nil || herr != nil || len(events) != 0 || !errors.Is(rerr, domain.ErrNotFound) {
					t.Fatalf("rejected connection mutation was overwritten or applied: origin=%s position=%+v events=%d errors=%v/%v/%v", origin, p, len(events), perr, herr, rerr)
				}
			}
			if _, err := os.Stat(filepath.Join(f.cwd, ".claude", "settings.json")); !os.IsNotExist(err) {
				t.Fatal("agent hooks activated after unresolved tracking")
			}
			binding, err := f.c.History.ResolveLocalBranch(context.Background(), f.repo, "local-task")
			if err != nil || binding.Tracking {
				t.Fatalf("failed setup adopted remote identity: %+v %v", binding, err)
			}
		})
	}
}

func TestSetupExistingCaptureAndRegistrationRemainUnchanged(t *testing.T) {
	f := setupTrackingFixture(t)
	ctx := context.Background()
	if _, err := f.c.Init.Init(ctx, inbound.InitInput{Cwd: f.cwd}); err != nil {
		t.Fatal(err)
	}
	pending := domain.Pending{SessionID: "already-captured", RepoID: string(domain.HashContent([]byte("provisional"))), Branch: "local-task", Target: f.a}
	if err := f.store.PutPending(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Sync.Connect(ctx, inbound.SyncInput{Cwd: f.cwd}); err != nil {
		t.Fatal(err)
	}
	if f.remote.pulls != 0 {
		t.Fatal("plain Connect performed tracking")
	}
	if err := runSetup(ctx, f.c, f.cwd, []string{"--no-login"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.ListPendings(ctx, "")
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], pending) || f.remote.pulls != 0 {
		t.Fatalf("existing capture changed: %+v %v", got, err)
	}
}

func (s *setupConnection) ResolveRemoteBranchObservation(ctx context.Context, in inbound.SyncInput, branch string) (inbound.RemoteBranchObservation, error) {
	got, err := s.tracking290Sync.ResolveRemoteBranchObservation(ctx, in, branch)
	if s.after != nil {
		s.after()
	}
	if s.failAfter != nil {
		return inbound.RemoteBranchObservation{}, s.failAfter
	}
	return got, err
}

func TestSetupFirstTrackingAllowsOnlyEmptyProvisionalPosition(t *testing.T) {
	f := setupTrackingFixture(t)
	provisional := string(domain.HashContent([]byte("Git-only identity before remote add")))
	if err := f.store.PutWorkingPosition(context.Background(), domain.WorkingPosition{RepoID: provisional, Branch: "local-task", GitCommit: f.oid}); err != nil {
		t.Fatal(err)
	}
	if err := runSetup(context.Background(), f.c, f.cwd, []string{"--no-login"}); err != nil {
		t.Fatal(err)
	}
	p, err := f.store.GetWorkingPosition(context.Background())
	if err != nil || p.RepoID != f.repo || p.BranchID != "remote-task-R" || p.Snapshot != f.a {
		t.Fatalf("provisional setup failed: %+v %v", p, err)
	}
}

// Reproduce the on-disk boundary of interruption after object transfer but
// before the observation receipt. No provider or remote process is involved.
func TestSetupFirstTrackingInterruptedFetchDoesNotBecomeSuccessfulSkip(t *testing.T) {
	f := setupTrackingFixture(t)
	ctx := context.Background()
	conn := f.c.Sync.(*setupConnection)
	conn.after = func() {
		if err := os.RemoveAll(filepath.Join(f.cwd, ".cxt", "remote-observations")); err != nil {
			t.Fatal(err)
		}
	}
	conn.failAfter = context.DeadlineExceeded
	if err := runSetup(ctx, f.c, f.cwd, []string{"--no-login"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first interruption: %v", err)
	}
	if _, err := f.store.GetSnapshot(ctx, f.a); err != nil {
		t.Fatal("interruption fixture lost imported object", err)
	}
	conn.after = nil
	conn.failAfter = nil
	if err := runSetup(ctx, f.c, f.cwd, []string{"--no-login"}); err == nil || !strings.Contains(err.Error(), "unproven local snapshot") {
		t.Fatalf("unproven cache became successful skip: %v", err)
	}
	if f.remote.pulls != 1 {
		t.Fatalf("unproven admission performed fresh network: %d", f.remote.pulls)
	}
	if _, err := os.Stat(filepath.Join(f.cwd, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatal("agent hooks activated")
	}
}
