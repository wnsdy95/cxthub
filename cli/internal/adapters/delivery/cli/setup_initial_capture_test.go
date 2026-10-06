package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Fake only the server transport. Setup, Connect, receipt verification,
// observation dispatch, capture/staging and FileStore are actual implementations.
type setupInitialRemote struct {
	outbound.RemoteSync
	repo                   domain.Repo
	receipt                *domain.RepositoryInitializationReceipt
	begins, plans, queries int
	unavailable            bool
	queryErr               error
	alter                  func(*outbound.RepositoryInitializationState)
	afterQuery             func()
}

func (r *setupInitialRemote) RepositoryInitializationState(_ context.Context, repo, branch string) (outbound.RepositoryInitializationState, error) {
	if r.receipt == nil {
		return outbound.RepositoryInitializationState{}, domain.ErrNotFound
	}
	if repo != r.repo.ID {
		return outbound.RepositoryInitializationState{}, domain.ErrHashMismatch
	}
	state := outbound.RepositoryInitializationState{Repo: r.repo, InitialAnchorAvailable: branch != "" && !r.unavailable}
	if branch != "" {
		r.queries++
		if branch != "local-task" {
			return outbound.RepositoryInitializationState{}, domain.ErrInvalidRef
		}
		if r.queryErr != nil {
			return outbound.RepositoryInitializationState{}, r.queryErr
		}
		if r.alter != nil {
			r.alter(&state)
		}
		if r.afterQuery != nil {
			r.afterQuery()
		}
	}
	return state, nil
}
func (r *setupInitialRemote) BeginRepositoryInitialization(_ context.Context, repo domain.Repo) (domain.RepositoryInitializationReceipt, error) {
	r.begins++
	if r.receipt == nil {
		repo.LocalPath, repo.RepositoryID, repo.ContextProtocol = "", "synthetic-server-owner", 1
		r.repo = repo
		r.receipt = &domain.RepositoryInitializationReceipt{Version: 1, CreationID: "init_" + strings.Repeat("a", 32), Repo: repo}
	}
	return *r.receipt, nil
}
func (r *setupInitialRemote) ReadRepositoryInitialization(context.Context, string) (domain.RepositoryInitializationReceipt, error) {
	return *r.receipt, nil
}
func (*setupInitialRemote) FinalizeRepositoryInitialization(context.Context, string, domain.RepositoryInitializationFinalize) (domain.RepositoryInitializationReceipt, error) {
	return domain.RepositoryInitializationReceipt{}, errors.New("setup must not finalize or invent an anchor")
}
func (r *setupInitialRemote) RegisterRepo(context.Context, domain.Repo) (domain.Repo, error) {
	return r.repo, nil
}
func (*setupInitialRemote) PullCapabilities(context.Context, string) (outbound.PullCapabilities, error) {
	return outbound.PullCapabilities{ContextProtocol: 1, BranchPlanVersion: domain.BranchPullVersion}, nil
}
func (r *setupInitialRemote) PullSelectedBranchTo(context.Context, string, domain.BranchPullRequest, map[domain.ContentHash]domain.ContentHash, []domain.ContentHash, outbound.PullDocumentReceiver) (domain.BranchPullPlan, []domain.Snapshot, error) {
	r.plans++
	return domain.BranchPullPlan{}, nil, domain.ErrNotFound
}
func setupInitialFixture(t *testing.T) (*tracking290Fixture, *setupInitialRemote, string) {
	t.Helper()
	f := setupTrackingFixture(t)
	url, _ := remotecfg.Origin(f.cwd)
	if err := os.Remove(filepath.Join(f.cwd, ".cxt", "config")); err != nil {
		t.Fatal(err)
	}
	runLifecycleGit(t, f.cwd, "update-ref", "refs/remotes/origin/local-task", f.oid)
	runLifecycleGit(t, f.cwd, "config", "branch.local-task.merge", "refs/heads/local-task")
	r := &setupInitialRemote{}
	f.c.Sync = app.NewSyncRepoService(f.store, r, remotecfg.Wrap(f.cwd, gitctx.NewGitContextAdapter()), storage.NewSyncOutbox())
	return f, r, url
}
func assertSetupEmpty(t *testing.T, f *tracking290Fixture) domain.WorkingPosition {
	t.Helper()
	ctx := context.Background()
	p, err := f.store.GetWorkingPosition(ctx)
	if err != nil || p.RepoID != f.repo || p.Branch != "local-task" || p.BranchID != domain.LegacyContextBranchID(f.repo, "local-task") || p.LocalBranch != "" || p.GitCommit != f.oid || p.Snapshot != "" || p.SharedTarget != "" || p.MemoryHash != "" || p.MemorySource != "" || p.Selection != nil || p.MemoryPinned || p.Orphan || p.Rewound {
		t.Fatalf("noncanonical or nonempty first position: %+v %v", p, err)
	}
	refs, err := f.store.ListRefs(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if ref.Kind != domain.RefHEAD {
			t.Fatalf("setup created ref: %+v", ref)
		}
	}
	events, err := f.store.ListHistoryEvents(ctx, f.repo)
	if err != nil || len(events) != 0 {
		t.Fatalf("setup created history: %+v %v", events, err)
	}
	binding, err := f.c.History.ResolveLocalBranch(ctx, f.repo, "local-task")
	if err != nil || binding.Tracking {
		t.Fatalf("setup created attachment: %+v %v", binding, err)
	}
	return p
}
func TestSetupInitialCaptureCanonicalAndRepeat(t *testing.T) {
	f, r, url := setupInitialFixture(t)
	ctx := context.Background()
	if _, ok := remotecfg.Origin(f.cwd); ok {
		t.Fatal("prior CXT origin")
	}
	if err := runSetup(ctx, f.c, f.cwd, []string{url, "--no-login"}); err != nil {
		t.Fatal(err)
	}
	before := assertSetupEmpty(t, f)
	if r.begins != 1 || r.queries != 1 || r.plans != 0 || r.receipt.Validate(f.repo) != nil {
		t.Fatalf("eligibility path: %+v", r)
	}
	if err := runSetup(ctx, f.c, f.cwd, []string{url, "--no-login"}); err != nil {
		t.Fatal(err)
	}
	if after := assertSetupEmpty(t, f); !reflect.DeepEqual(before, after) {
		t.Fatal("retry changed position")
	}
	// Native-format fixture only: no provider executable, Git move or repair.
	path := filepath.Join(f.cwd, "first-session.jsonl")
	appendStagingPrompt(t, path, domain.ProviderClaude, f.cwd, "first-session", "synthetic first capture at unchanged code")
	gc := remotecfg.Wrap(f.cwd, gitctx.NewGitContextAdapter())
	captures := map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture()}
	codecs := map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec()}
	save := app.NewSaveSessionService(gc, captures, codecs, f.store, capture.NewSessionCapture(f.store), storage.NewSyncOutbox())
	if _, err := save.Save(ctx, inbound.SaveInput{Cwd: f.cwd, Provider: domain.ProviderClaude, SessionPath: path, Pending: true}); err != nil {
		t.Fatal("first capture", err)
	}
	stage := app.NewStagingService(gc, gitctx.NewGitContextAdapter(), f.store, f.store, captures, codecs, capture.NewSessionCapture(f.store), storage.NewSyncOutbox())
	index, err := stage.Stage(ctx, inbound.StageInput{Cwd: f.cwd, Sessions: []inbound.StageSession{{Provider: domain.ProviderClaude, Path: path, SessionID: "first-session"}}})
	if err != nil {
		t.Fatal("first add", err)
	}
	if _, err = stage.Commit(ctx, inbound.StagingCommitInput{Cwd: f.cwd, Message: "first owner", ExpectedRevision: index.Revision}); err != nil {
		t.Fatal("first commit", err)
	}
	captured, err := f.store.GetWorkingPosition(ctx)
	if err != nil || captured.Snapshot == "" || captured.RepoID != f.repo || captured.BranchID != before.BranchID || captured.GitCommit != f.oid {
		t.Fatalf("capture selection: %+v %v", captured, err)
	}
	if gitOut(f.cwd, "rev-parse", "HEAD") != f.oid {
		t.Fatal("test repaired Git code")
	}
	queries := r.queries
	if err := runSetup(ctx, f.c, f.cwd, []string{url, "--no-login"}); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(captured, after) || r.queries != queries {
		t.Fatalf("captured retry changed/refetched: %+v %v", after, err)
	}
}
func TestSetupInitialCaptureEligibilityAndAdmission(t *testing.T) {
	cases := []string{"unavailable", "query-not-found", "query-auth", "query-network", "wrong-repo", "protocol-zero", "empty-origin", "wrong-origin", "changed-remote-url", "alias", "Git-change", "connection-change", "empty-position-change", "pending-capture", "other-worktree", "snapshot"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			f, r, url := setupInitialFixture(t)
			ctx := context.Background()
			wantPlans := 0
			switch name {
			case "unavailable":
				r.unavailable = true
				wantPlans = 1
			case "query-not-found":
				r.queryErr = domain.ErrNotFound
			case "query-auth":
				r.queryErr = errors.New("synthetic forbidden")
			case "query-network":
				r.queryErr = context.DeadlineExceeded
			case "wrong-repo":
				r.alter = func(s *outbound.RepositoryInitializationState) {
					s.Repo.ID = string(domain.HashContent([]byte("other")))
				}
			case "protocol-zero":
				r.alter = func(s *outbound.RepositoryInitializationState) { s.Repo.ContextProtocol = 0 }
			case "empty-origin":
				r.alter = func(s *outbound.RepositoryInitializationState) { s.Repo.GitRemoteURL = "" }
			case "wrong-origin":
				r.alter = func(s *outbound.RepositoryInitializationState) {
					s.Repo.GitRemoteURL = "https://example.invalid/other/code"
				}
			case "changed-remote-url":
				r.alter = func(s *outbound.RepositoryInitializationState) {
					s.Repo.RemoteURL = "https://example.invalid/other/context"
				}
			case "alias":
				runLifecycleGit(t, f.cwd, "config", "branch.local-task.merge", "refs/heads/team-task")
				wantPlans = 1
			case "Git-change":
				r.afterQuery = func() { runLifecycleGit(t, f.cwd, "config", "branch.local-task.merge", "refs/heads/team-task") }
			case "connection-change":
				r.afterQuery = func() {
					if err := configFixtureSave(f.cwd, remotecfg.Remotes{"origin": "https://example.invalid/other/context"}); err != nil {
						t.Fatal(err)
					}
				}
			case "empty-position-change":
				r.afterQuery = func() {
					p, e := f.store.GetWorkingPosition(ctx)
					if e != nil {
						t.Fatal(e)
					}
					p.Branch = "changed"
					if e = f.store.PutWorkingPosition(ctx, p); e != nil {
						t.Fatal(e)
					}
				}
			case "pending-capture":
				r.afterQuery = func() {
					if e := f.store.PutPending(ctx, domain.Pending{SessionID: "racing", RepoID: f.repo, Branch: "local-task", Target: f.a}); e != nil {
						t.Fatal(e)
					}
				}
			case "other-worktree":
				r.afterQuery = func() {
					peer := storage.NewWorktreeFileStore(f.cwd, filepath.Join(f.cwd, "other-admin"), "other", f.oid)
					if e := peer.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: f.repo, Branch: "other", GitCommit: f.oid, Snapshot: f.a}); e != nil {
						t.Fatal(e)
					}
				}
			case "snapshot":
				r.afterQuery = func() {
					id, e := f.store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{}})
					if e != nil {
						t.Fatal(e)
					}
					if e = f.store.PutSnapshot(ctx, domain.Snapshot{RepoID: f.repo, ID: id, DocHash: id, Branch: "local-task"}); e != nil {
						t.Fatal(e)
					}
				}
			}
			err := runSetup(ctx, f.c, f.cwd, []string{url, "--no-login"})
			if err == nil || r.plans != wantPlans {
				t.Fatalf("unauthorized empty path: err=%v plans=%d want=%d", err, r.plans, wantPlans)
			}
			if _, err := os.Stat(filepath.Join(f.cwd, ".claude", "settings.json")); !os.IsNotExist(err) {
				t.Fatal("enabled agent hooks")
			}
			if name == "alias" && r.queries != 0 {
				t.Fatal("queried unpublished alias eligibility")
			}
		})
	}
}

// The real app's absent optional server capability preserves populated tracking.
func TestSetupInitialCaptureAbsentCapabilityKeepsPopulatedObservation(t *testing.T) {
	f := setupTrackingFixture(t)
	runLifecycleGit(t, f.cwd, "update-ref", "refs/remotes/origin/local-task", f.oid)
	runLifecycleGit(t, f.cwd, "config", "branch.local-task.merge", "refs/heads/local-task")
	f.remote.ref.Name = "local-task"
	for i := range f.remote.history {
		f.remote.history[i].Branch = "local-task"
	}
	for i := range f.remote.snaps {
		f.remote.snaps[i].Branch = "local-task"
	}
	conn := f.c.Sync.(*setupConnection)
	// Forward only the new query to the real app, preserving this fixture's fake Connect.
	f.c.Sync = &setupNoInitialCapability{setupConnection: conn}
	if err := runSetup(context.Background(), f.c, f.cwd, []string{"--no-login"}); err != nil {
		t.Fatal(err)
	}
	if f.remote.pulls != 1 {
		t.Fatalf("populated path skipped: %d", f.remote.pulls)
	}
}

type setupNoInitialCapability struct{ *setupConnection }

func (s *setupNoInitialCapability) InitialCaptureEligibility(ctx context.Context, in inbound.SyncInput, branch string) (domain.Repo, bool, error) {
	return s.tracking290Sync.SyncRepo.(inbound.SetupInitialCapture).InitialCaptureEligibility(ctx, in, branch)
}
