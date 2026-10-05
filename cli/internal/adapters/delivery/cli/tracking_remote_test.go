package cli

// Only the deterministic
// remote transport is fake; fetch, verification, FileStore, CLI resolution and
// application are the existing implementations. Git touches t.TempDir only.
import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type tracking290Remote struct {
	outbound.RemoteSync
	ref                         domain.Ref
	snaps                       []domain.Snapshot
	docs                        []domain.SessionDoc
	history                     []domain.HistoryEvent
	memories                    map[domain.ContentHash]domain.MemoryDigest
	pulls, histories, manifests int
}

func (r *tracking290Remote) ContextProtocol(context.Context, string) (int, error) { return 0, nil }
func (r *tracking290Remote) RemoteManifest(context.Context, string) (domain.Manifest, error) {
	r.manifests++
	return domain.Manifest{}, fmt.Errorf("unexpected extra manifest")
}
func (r *tracking290Remote) Pull(_ context.Context, repo string, _ map[domain.ContentHash]domain.ContentHash, _ []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	r.pulls++
	if repo != r.ref.RepoID {
		return nil, nil, nil, fmt.Errorf("unexpected repository")
	}
	return r.snaps, r.docs, []domain.Ref{r.ref}, nil
}
func (r *tracking290Remote) PullHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error) {
	r.histories++
	return r.history, nil
}
func (*tracking290Remote) PushHistoryEvent(context.Context, domain.HistoryEvent) error {
	return fmt.Errorf("resolution must not publish history")
}
func (r *tracking290Remote) PullMemory(_ context.Context, _ string, snapshot domain.ContentHash) (domain.MemoryDigest, error) {
	for _, snap := range r.snaps {
		if snap.ID == snapshot {
			return r.PullMemoryObject(context.Background(), "", snap.MemoryHash)
		}
	}
	return domain.MemoryDigest{}, domain.ErrNotFound
}
func (r *tracking290Remote) PullMemoryObject(_ context.Context, _ string, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if memory, ok := r.memories[hash]; ok {
		return memory, nil
	}
	return domain.MemoryDigest{}, domain.ErrNotFound
}

// Observe the actual existing Ref-only API without fabricating its result.
type tracking290Sync struct {
	inbound.SyncRepo
	ref   domain.Ref
	err   error
	calls int
}

func (s *tracking290Sync) ResolveRemoteBranch(ctx context.Context, in inbound.SyncInput, branch string) (domain.Ref, error) {
	s.calls++
	s.ref, s.err = s.SyncRepo.ResolveRemoteBranch(ctx, in, branch)
	return s.ref, s.err
}

func (s *tracking290Sync) ResolveRemoteBranchObservation(ctx context.Context, in inbound.SyncInput, branch string) (inbound.RemoteBranchObservation, error) {
	s.calls++
	got, err := s.SyncRepo.(inbound.RemoteBranchObserver).ResolveRemoteBranchObservation(ctx, in, branch)
	s.ref, s.err = got.Ref, err
	return got, err
}

type tracking290Fixture struct {
	cwd, repo, oid, gitdir string
	c                      *Container
	store                  *storage.FileStore
	remote                 *tracking290Remote
	sync                   *tracking290Sync
	op                     branchjournal.Operation
	a, b, m1, m2           domain.ContentHash
}

func newTracking290Fixture(t *testing.T, label bool) *tracking290Fixture {
	t.Helper()
	cwd, c, _, repo, _ := historyFixture(t)
	runLifecycleGit(t, cwd, "checkout", "-qb", "local-task")
	f := &tracking290Fixture{cwd: cwd, repo: repo, c: c, oid: gitOut(cwd, "rev-parse", "HEAD"), gitdir: gitOut(cwd, "rev-parse", "--absolute-git-dir")}
	f.store = storage.NewWorktreeFileStore(cwd, f.gitdir, "local-task", f.oid)
	c.List = app.NewListSessionsService(f.store)
	c.Fork = app.NewForkSessionService(f.store)
	c.History = app.NewContextHistoryService(f.store, f.store)
	makeDoc := func(text string) domain.SessionDoc {
		cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull}, Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}}
		data, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		return domain.SessionDoc{Hash: domain.HashContent(data), CIR: cir}
	}
	da, db := makeDoc("remote selected A"), makeDoc("remote shared tip B")
	f.a, f.b = da.Hash, db.Hash
	m1 := domain.MemoryDigest{SnapshotID: f.a, Summary: "M1 pinned at frozen Git association"}
	var err error
	f.m1, err = domain.MemoryDigestHash(m1)
	if err != nil {
		t.Fatal(err)
	}
	m2 := domain.MemoryDigest{SnapshotID: f.a, Summary: "M2 later mutable attachment", PreviousMemoryHash: f.m1}
	f.m2, err = domain.MemoryDigestHash(m2)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	message := "no legacy Git label"
	if label {
		message = "legacy compatibility label [git " + f.oid + "]"
	}
	birth := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: "remote-task-R", Branch: "team-task", Kind: "birth", Source: f.a, Target: f.a, MemoryHash: f.m1, MemorySource: f.a, MemoryPinned: true, CreatedAt: when}
	association := domain.HistoryEvent{ID: strings.Repeat("b", 32), RepoID: repo, BranchID: birth.BranchID, Branch: birth.Branch, Kind: "advance", Source: f.a, Target: f.a, GitBefore: f.oid, GitAfter: f.oid, MemoryHash: f.m1, MemorySource: f.a, MemoryPinned: true, CreatedAt: when.Add(time.Second)}
	f.remote = &tracking290Remote{
		ref:   domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "team-task", BranchID: birth.BranchID, Target: f.b},
		snaps: []domain.Snapshot{{ID: f.a, DocHash: f.a, RepoID: repo, Branch: "team-task", Message: message, MemoryHash: f.m2, CreatedAt: when}, {ID: f.b, DocHash: f.b, RepoID: repo, Branch: "team-task", Parents: []domain.ContentHash{f.a}, CreatedAt: when.Add(time.Second)}},
		docs:  []domain.SessionDoc{da, db}, history: []domain.HistoryEvent{birth, association}, memories: map[domain.ContentHash]domain.MemoryDigest{f.m1: m1, f.m2: m2},
	}
	for _, event := range f.remote.history {
		if err := domain.ValidateHistoryEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	projection, err := domain.ProjectContextBranches(f.remote.history)
	if err != nil || projection.Active["team-task"].ID != birth.BranchID {
		t.Fatalf("invalid remote proof: %+v %v", projection, err)
	}
	realSync := app.NewSyncRepoService(f.store, f.remote, remotecfg.Wrap(cwd, gitctx.NewGitContextAdapter()), storage.NewSyncOutbox())
	f.sync = &tracking290Sync{SyncRepo: realSync}
	c.Sync = f.sync
	wt := sha256.Sum256([]byte(f.gitdir))
	f.op = branchjournal.Operation{Phase: "committed", GitRef: "refs/heads/local-task", Worktree: cwd, Binding: &branchjournal.BindingIntent{Kind: "attach", RemoteBranch: "team-task"}, Event: domain.HistoryEvent{ID: strings.Repeat("c", 32), RepoID: repo, BranchID: "prepared-local-task", Branch: "local-task", Kind: "birth", GitAfter: f.oid, WorktreeID: fmt.Sprintf("%x", wt[:16]), CreatedAt: when.Add(2 * time.Second)}}
	return f
}

// Assert the real fetch completed, retained the remote proof, and did not adopt
// it into local history/ref/position. Setup/transport failure is NOT our red.
func (f *tracking290Fixture) resolve(t *testing.T, wantLocalHistory int) (domain.HistoryEvent, error) {
	t.Helper()
	ctx := context.Background()
	beforeRefs, err := f.store.ListRefs(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := f.store.ListHistoryEvents(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeHistory) != wantLocalHistory {
		t.Fatalf("local-history precondition: got %d events, want %d", len(beforeHistory), wantLocalHistory)
	}
	beforePosition, beforePositionErr := f.store.GetWorkingPosition(ctx)
	got, proof, resolveErr := resolveBranchOperation(ctx, f.c, f.cwd, f.op)
	f.op.Tracking = proof
	if f.sync.err != nil || f.sync.ref != f.remote.ref || f.sync.calls != 1 || f.remote.pulls != 1 || f.remote.histories != 1 || f.remote.manifests != 0 {
		t.Fatalf("FETCH PRECONDITION failed: fresh=%+v err=%v calls=%d pull/history/manifest=%d/%d/%d", f.sync.ref, f.sync.err, f.sync.calls, f.remote.pulls, f.remote.histories, f.remote.manifests)
	}
	observed, err := f.store.ReadRemoteObservation(ctx, f.repo, "configured")
	if err != nil || !reflect.DeepEqual(observed.History, f.remote.history) {
		t.Fatalf("FETCH PRECONDITION: missing remote proof: %+v %v", observed.History, err)
	}
	for _, hash := range []domain.ContentHash{f.m1, f.m2} {
		if _, err := f.store.GetMemory(ctx, hash); err != nil {
			t.Fatalf("missing verified memory %s: %v", hash, err)
		}
	}
	afterRefs, err := f.store.ListRefs(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	afterHistory, err := f.store.ListHistoryEvents(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	afterPosition, afterPositionErr := f.store.GetWorkingPosition(ctx)
	if !reflect.DeepEqual(beforeRefs, afterRefs) || !reflect.DeepEqual(beforeHistory, afterHistory) || !reflect.DeepEqual(beforePosition, afterPosition) || fmt.Sprint(beforePositionErr) != fmt.Sprint(afterPositionErr) {
		t.Fatal("resolution changed locally applied refs/history/position")
	}
	t.Logf("verified fresh ref identity=%s target=B; one pull + one history read; observed_history=%d local_history=%d; both M1/M2 retained; applied state unchanged", f.sync.ref.BranchID, len(observed.History), len(afterHistory))
	return got, resolveErr
}

func (f *tracking290Fixture) checkSelection(t *testing.T, got domain.HistoryEvent) {
	t.Helper()
	if got.BranchID != "remote-task-R" || got.Source != f.a || got.Target != f.a || got.SharedTarget != f.b || got.MemoryHash != f.m1 || got.MemorySource != "" || !got.MemoryPinned {
		t.Errorf("wanted R/A/shared-B/pinned-M1; got identity=%s sourceA=%t targetA=%t sharedB=%t memoryM1=%t memoryM2=%t canonicalSelfOwner=%t pinned=%t", got.BranchID, got.Source == f.a, got.Target == f.a, got.SharedTarget == f.b, got.MemoryHash == f.m1, got.MemoryHash == f.m2, got.MemorySource == "", got.MemoryPinned)
	}
}

func TestTracking290RemoteOnlyProofWithoutLabel(t *testing.T) {
	f := newTracking290Fixture(t, false)
	got, err := f.resolve(t, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.checkSelection(t, got)
	f.op.Event, f.op.Resolved = got, true
	if err := applyBranchOperation(context.Background(), f.c, f.cwd, f.op, true); err != nil {
		t.Fatal(err)
	}
	binding, err := f.store.ResolveLocalBranch(context.Background(), f.repo, "local-task")
	if err != nil || binding.BranchID != "remote-task-R" {
		t.Fatalf("%+v %v", binding, err)
	}
}

func TestTracking290LegacyLabelMustNotOverrideRemoteProof(t *testing.T) {
	for _, localBirth := range []bool{false, true} {
		t.Run(fmt.Sprintf("local_birth=%t", localBirth), func(t *testing.T) {
			f := newTracking290Fixture(t, true)
			wantLocalHistory := 0
			if localBirth {
				if err := f.store.PutHistoryEvent(context.Background(), f.remote.history[0]); err != nil {
					t.Fatal(err)
				}
				wantLocalHistory = 1
			}
			got, err := f.resolve(t, wantLocalHistory)
			if err != nil {
				t.Fatal(err)
			}
			f.checkSelection(t, got)
			// A wrong successful selection must also be caught after real apply
			// and reopen, not only in the resolver's returned event.
			f.op.Event, f.op.Resolved = got, true
			if err := applyBranchOperation(context.Background(), f.c, f.cwd, f.op, true); err != nil {
				t.Fatalf("apply failed: %v", err)
			}
			reopened := storage.NewWorktreeFileStore(f.cwd, f.gitdir, "local-task", f.oid)
			binding, bindErr := reopened.ResolveLocalBranch(context.Background(), f.repo, "local-task")
			ref, refErr := reopened.GetRef(context.Background(), f.repo, domain.RefBranch, "team-task")
			position, posErr := reopened.GetWorkingPosition(context.Background())
			if bindErr != nil || refErr != nil || posErr != nil {
				t.Fatalf("reopen errors: binding=%v ref=%v position=%v", bindErr, refErr, posErr)
			}
			if binding.BranchID != "remote-task-R" || ref.BranchID != "remote-task-R" || position.BranchID != "remote-task-R" || position.Snapshot != f.a || position.SharedTarget != f.b || position.MemoryHash != f.m1 || !position.MemoryPinned {
				t.Errorf("persisted identity/pin mismatch: binding=%s ref=%s position=%s snapshotA=%t sharedB=%t M1=%t M2=%t", binding.BranchID, ref.BranchID, position.BranchID, position.Snapshot == f.a, position.SharedTarget == f.b, position.MemoryHash == f.m1, position.MemoryHash == f.m2)
			}
		})
	}
}

// Green control isolates the missing fetch-to-caller history handoff: the same
// valid remote proof resolves correctly if explicitly installed locally first.
func TestTracking290LocalHistoryControl(t *testing.T) {
	f := newTracking290Fixture(t, false)
	for _, event := range f.remote.history {
		if err := f.store.PutHistoryEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.resolve(t, 2)
	if err != nil {
		t.Fatal(err)
	}
	f.checkSelection(t, got)
}

func (f *tracking290Fixture) journal(t *testing.T) *branchjournal.Journal {
	t.Helper()
	j, err := branchjournal.Open(context.Background(), f.cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Enable(); err != nil {
		t.Fatal(err)
	}
	if err := j.Transaction(context.Background(), func() error { return j.Save(f.op) }); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestTracking290ConflictStaysDurableAndDoesNotInventLegacyIdentity(t *testing.T) {
	f := newTracking290Fixture(t, true)
	f.remote.ref.BranchID = "different-identity"
	j := f.journal(t)
	if err := replayBranchOperations(context.Background(), f.c, f.cwd); err == nil {
		t.Fatal("identity mismatch accepted")
	}
	ops, err := j.List()
	if err != nil || len(ops) != 1 {
		t.Fatalf("%+v %v", ops, err)
	}
	if ops[0].Resolved || ops[0].Phase != "committed" || ops[0].LastError == "" || ops[0].Tracking != nil {
		t.Fatalf("unsafe operation acknowledged: %+v", ops[0])
	}
	events, err := f.store.ListHistoryEvents(context.Background(), f.repo)
	if err != nil || len(events) != 0 {
		t.Fatalf("failed observation adopted history: %+v %v", events, err)
	}
	if _, err := f.store.GetRef(context.Background(), f.repo, domain.RefBranch, "team-task"); err != domain.ErrNotFound {
		t.Fatalf("failed attach created ref: %v", err)
	}
}

func TestTracking290ReplayUsesFrozenProofAndCompletedReceipt(t *testing.T) {
	f := newTracking290Fixture(t, false)
	got, err := f.resolve(t, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.op.Event, f.op.Resolved = got, true
	j := f.journal(t)
	// A different future observation cannot change the committed resolution.
	f.remote.history = nil
	f.remote.ref.BranchID = "newer-remote-identity"
	if err := applyBranchOperation(context.Background(), f.c, f.cwd, f.op, true); err != nil {
		t.Fatal(err)
	}
	// Simulate store success before the Git journal's applied acknowledgement.
	// Another worktree can advance the shared ref before that acknowledgement.
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "after accepted tracking"}}}
	id, err := f.store.PutDoc(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutSnapshot(context.Background(), domain.Snapshot{ID: id, DocHash: id, RepoID: f.repo, Branch: "team-task", Parents: []domain.ContentHash{f.b}}); err != nil {
		t.Fatal(err)
	}
	after := domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "team-task", BranchID: got.BranchID, Target: id}
	if err := f.store.PutRef(context.Background(), after); err != nil {
		t.Fatal(err)
	}
	position, err := f.store.GetWorkingPosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := replayBranchOperations(context.Background(), f.c, f.cwd); err != nil {
		t.Fatal(err)
	}
	ops, err := j.List()
	if err != nil || len(ops) != 1 || ops[0].Phase != "applied" {
		t.Fatalf("acknowledgment: %+v %v", ops, err)
	}
	if f.sync.calls != 1 {
		t.Fatalf("replay refetched mutable remote evidence %d times", f.sync.calls)
	}
	ref, err := f.store.GetRef(context.Background(), f.repo, domain.RefBranch, "team-task")
	if err != nil || ref != after {
		t.Fatalf("receipt replay rewound newer ref: %+v %v", ref, err)
	}
	now, err := f.store.GetWorkingPosition(context.Background())
	if err != nil || !reflect.DeepEqual(now, position) {
		t.Fatalf("receipt replay changed selected position: %+v %v", now, err)
	}
}

func TestTracking290PinnedEmptyDoesNotInheritMutableMemory(t *testing.T) {
	f := newTracking290Fixture(t, false)
	for i := range f.remote.history {
		f.remote.history[i].MemoryHash = ""
		f.remote.history[i].MemorySource = ""
	}
	got, err := f.resolve(t, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.MemoryPinned || got.MemoryHash != "" {
		t.Fatalf("mutable memory inherited: %+v", got)
	}
	f.op.Event, f.op.Resolved = got, true
	if err := applyBranchOperation(context.Background(), f.c, f.cwd, f.op, true); err != nil {
		t.Fatal(err)
	}
	p, err := f.store.GetWorkingPosition(context.Background())
	if err != nil || !p.MemoryPinned || p.MemoryHash != "" {
		t.Fatalf("empty memory not preserved: %+v %v", p, err)
	}
}

func TestTracking290MemorySelectionUsesCausalHistoryNotClocks(t *testing.T) {
	for _, mode := range []string{"causal successor with older clock", "divergent memories", "empty and nonempty"} {
		t.Run(mode, func(t *testing.T) {
			f := newTracking290Fixture(t, false)
			e := f.remote.history[1]
			e.ID = strings.Repeat("d", 32)
			e.CreatedAt = e.CreatedAt.Add(-time.Hour)
			switch mode {
			case "causal successor with older clock":
				e.MemoryHash = f.m2
			case "divergent memories":
				d := domain.MemoryDigest{SnapshotID: f.a, Summary: "independent memory branch"}
				hash, err := domain.MemoryDigestHash(d)
				if err != nil {
					t.Fatal(err)
				}
				e.MemoryHash = hash
				f.remote.memories[hash] = d
			case "empty and nonempty":
				e.MemoryHash = ""
				e.MemorySource = ""
			}
			f.remote.history = append(f.remote.history, e)
			// Use the real query, but this scenario deliberately changes the history
			// length rather than the basic fixture's two-event assertion.
			got, proof, err := resolveBranchOperation(context.Background(), f.c, f.cwd, f.op)
			if mode == "causal successor with older clock" {
				if err != nil || got.MemoryHash != f.m2 || proof == nil {
					t.Fatalf("causal maximum: %+v %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("ambiguous memory accepted: %+v", got)
			}
			events, readErr := f.store.ListHistoryEvents(context.Background(), f.repo)
			if readErr != nil || len(events) != 0 {
				t.Fatalf("query applied evidence: %+v %v", events, readErr)
			}
		})
	}
}
