package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type appendBranchRemote struct {
	outbound.RemoteSync
	snapshots map[domain.ContentHash]domain.Snapshot
	failGets  map[domain.ContentHash]int
	getCalls  []domain.ContentHash
	updates   int
	updateRef domain.Ref
	append    bool
	updateErr error
	deleted   map[string]domain.ContentHash
}

type conditionalAppendBranchRemote struct {
	*appendBranchRemote
	conditionalCalls int
	expectedTarget   domain.ContentHash
	expectedRevision domain.RepositoryRevision
}

func (remote *conditionalAppendBranchRemote) AppendRefConditional(_ context.Context, repo string, ref domain.Ref, target domain.ContentHash, revision domain.RepositoryRevision) error {
	remote.conditionalCalls++
	remote.updateRef = ref
	remote.expectedTarget = target
	remote.expectedRevision = revision
	return remote.updateErr
}

func TestAppendBranchConditional(t *testing.T) {
	fixture := newAppendBranchFixture(t)
	remote := &conditionalAppendBranchRemote{appendBranchRemote: fixture.remote}
	fixture.service.remote = remote
	revision := domain.RepositoryRevision{Graph: 4, Evidence: 8, Pending: 12}
	expected := domain.HashContent([]byte("server position differs from local"))
	in := inbound.SyncInput{RepoID: fixture.repoID, ExpectedBranchID: domain.LegacyContextBranchID(fixture.repoID, "main"), ExpectedTarget: expected, ExpectedRevision: &revision}
	if err := fixture.service.AppendBranch(context.Background(), in, "main", fixture.target); err != nil {
		t.Fatal(err)
	}
	if remote.conditionalCalls != 1 || remote.updates != 0 || remote.expectedTarget != expected || remote.expectedRevision != revision || remote.updateRef.BranchID != in.ExpectedBranchID || remote.updateRef.Target != fixture.target {
		t.Fatalf("conditional binding lost: %+v", remote)
	}
	ref, err := fixture.store.GetRef(context.Background(), fixture.repoID, domain.RefBranch, "main")
	if err != nil || ref.Target != fixture.target {
		t.Fatalf("successful conditional append did not converge: %+v %v", ref, err)
	}
}

func TestAppendBranchConditionalFailsClosed(t *testing.T) {
	for _, mode := range []string{"unsupported", "missing revision", "missing target", "force", "stale", "old server", "recreated branch"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newAppendBranchFixture(t)
			remote := &conditionalAppendBranchRemote{appendBranchRemote: fixture.remote}
			fixture.service.remote = remote
			revision := domain.RepositoryRevision{Graph: 4, Evidence: 8}
			in := inbound.SyncInput{RepoID: fixture.repoID, ExpectedBranchID: domain.LegacyContextBranchID(fixture.repoID, "main"), ExpectedTarget: fixture.oldHead, ExpectedRevision: &revision}
			wantCalls := 0
			switch mode {
			case "unsupported":
				fixture.service.remote = fixture.remote
			case "missing revision":
				in.ExpectedRevision = nil
			case "missing target":
				in.ExpectedTarget = ""
			case "force":
				in.Force = true
			case "stale":
				remote.updateErr, wantCalls = domain.ErrSyncConflict, 1
			case "old server":
				remote.updateErr, wantCalls = domain.ErrNotFound, 1
			case "recreated branch":
				in.ExpectedBranchID = "previous-context"
			}
			if err := fixture.service.AppendBranch(context.Background(), in, "main", fixture.target); err == nil {
				t.Fatal("conditional failure accepted")
			}
			if remote.conditionalCalls != wantCalls || remote.updates != 0 || len(remote.getCalls) != 0 || len(remote.deleted) != 0 {
				t.Fatalf("failure fell back or started convergence: %+v", remote)
			}
			ref, err := fixture.store.GetRef(context.Background(), fixture.repoID, domain.RefBranch, "main")
			if err != nil || ref.Target != fixture.oldHead {
				t.Fatalf("failure changed local ref: %+v %v", ref, err)
			}
			boundary, err := fixture.store.GetSnapshot(context.Background(), fixture.boundary)
			if err != nil || boundary.Grafted || len(boundary.GraftParents) != 0 {
				t.Fatalf("failure changed local graft: %+v %v", boundary, err)
			}
		})
	}
}

func (r *appendBranchRemote) UpdateRefRemote(
	_ context.Context,
	_ string,
	ref domain.Ref,
	appendDiverged bool,
) error {
	r.updates++
	r.updateRef = ref
	r.append = appendDiverged
	return r.updateErr
}

func (r *appendBranchRemote) GetSnapshotRemote(
	_ context.Context,
	_ string,
	id domain.ContentHash,
) (domain.Snapshot, error) {
	r.getCalls = append(r.getCalls, id)
	if r.failGets[id] > 0 {
		r.failGets[id]--
		return domain.Snapshot{}, errors.New("temporary remote read failure")
	}
	snap, ok := r.snapshots[id]
	if !ok {
		return domain.Snapshot{}, domain.ErrNotFound
	}
	return snap, nil
}

func (r *appendBranchRemote) CompareAndDeletePendingRemote(_ context.Context, _ string, sessionID string, expected domain.ContentHash) (bool, error) {
	if r.deleted == nil {
		r.deleted = make(map[string]domain.ContentHash)
	}
	r.deleted[sessionID] = expected
	return true, nil
}

type appendBranchFixture struct {
	store    *storage.FileStore
	remote   *appendBranchRemote
	service  *SyncRepoService
	repoID   string
	oldHead  domain.ContentHash
	boundary domain.ContentHash
	target   domain.ContentHash
}

func newAppendBranchFixture(t *testing.T) appendBranchFixture {
	t.Helper()
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("append-branch-repo")))
	oldHead := domain.HashContent([]byte("append-old-head"))
	boundary := domain.HashContent([]byte("append-feature-boundary"))
	target := domain.HashContent([]byte("append-feature-target"))

	put := func(snap domain.Snapshot) {
		t.Helper()
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	put(domain.Snapshot{
		ID: oldHead, RepoID: repoID, Branch: "main", DocHash: oldHead,
	})
	put(domain.Snapshot{
		ID: boundary, RepoID: repoID, Branch: "feature", DocHash: boundary,
	})
	put(domain.Snapshot{
		ID: target, RepoID: repoID, Branch: "feature", DocHash: target,
		Parents: []domain.ContentHash{boundary},
	})
	if err := st.PutRef(ctx, domain.Ref{
		Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: oldHead,
	}); err != nil {
		t.Fatal(err)
	}

	remoteBoundary := domain.Snapshot{
		ID: boundary, RepoID: repoID, Branch: "feature", DocHash: boundary,
		Grafted: true, GraftParents: []domain.ContentHash{oldHead}, GraftSeq: 1,
	}
	remote := &appendBranchRemote{
		snapshots: map[domain.ContentHash]domain.Snapshot{
			oldHead: {
				ID: oldHead, RepoID: repoID, Branch: "main", DocHash: oldHead,
			},
			boundary: remoteBoundary,
			target: {
				ID: target, RepoID: repoID, Branch: "feature", DocHash: target,
				Parents: []domain.ContentHash{boundary}, GraftSeq: 9,
			},
		},
		failGets: map[domain.ContentHash]int{},
	}
	return appendBranchFixture{
		store: st, remote: remote, service: newTestSyncService(st, remote, nil),
		repoID: repoID, oldHead: oldHead, boundary: boundary, target: target,
	}
}

func TestAppendBranchReconcilesRemoteGraftPathAndAdvancesLocalRef(t *testing.T) {
	f := newAppendBranchFixture(t)
	ctx := context.Background()
	if err := f.store.PutPending(ctx, domain.Pending{
		RepoID: f.repoID, SessionID: "merged-session", Branch: "main", Target: f.boundary,
	}); err != nil {
		t.Fatal(err)
	}

	if err := f.service.AppendBranch(ctx, inbound.SyncInput{RepoID: f.repoID}, "main", f.target); err != nil {
		t.Fatal(err)
	}
	if f.remote.updates != 1 || !f.remote.append || f.remote.updateRef.Target != f.target {
		t.Fatalf("remote update = count:%d append:%v ref:%+v", f.remote.updates, f.remote.append, f.remote.updateRef)
	}
	wantCalls := []domain.ContentHash{f.target, f.boundary}
	if fmt.Sprint(f.remote.getCalls) != fmt.Sprint(wantCalls) {
		t.Fatalf("authoritative reads = %v, want %v", f.remote.getCalls, wantCalls)
	}

	ref, err := f.store.GetRef(ctx, f.repoID, domain.RefBranch, "main")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Target != f.target {
		t.Fatalf("local main = %s, want %s", ref.Target, f.target)
	}
	boundary, err := f.store.GetSnapshot(ctx, f.boundary)
	if err != nil {
		t.Fatal(err)
	}
	if len(boundary.Parents) != 0 {
		t.Fatalf("natural parents changed: %v", boundary.Parents)
	}
	if boundary.GraftSeq != 1 || !boundary.Grafted || len(boundary.GraftParents) != 1 || boundary.GraftParents[0] != f.oldHead {
		t.Fatalf("authoritative graft not adopted: %+v", boundary)
	}
	target, err := f.store.GetSnapshot(ctx, f.target)
	if err != nil {
		t.Fatal(err)
	}
	if target.GraftSeq != 0 {
		t.Fatalf("unrelated target graft register was overwritten: %+v", target)
	}
	if f.remote.deleted["merged-session"] != f.boundary {
		t.Fatalf("append did not reconcile newly shared pending: %v", f.remote.deleted)
	}
	pendings, err := f.store.ListPendings(ctx, f.repoID)
	if err != nil || len(pendings) != 0 {
		t.Fatalf("append left reachable local pending: %+v err=%v", pendings, err)
	}
}

func TestAppendBranchRejectsRecreatedBranchBeforeRemoteWrite(t *testing.T) {
	fixture := newAppendBranchFixture(t)
	ctx := context.Background()
	before, err := fixture.store.GetRef(ctx, fixture.repoID, domain.RefBranch, "main")
	if err != nil {
		t.Fatal(err)
	}
	err = fixture.service.AppendBranch(ctx, inbound.SyncInput{RepoID: fixture.repoID, ExpectedBranchID: "previous-branch-identity"}, "main", fixture.target)
	if !errors.Is(err, domain.ErrSyncConflict) || fixture.remote.updates != 0 {
		t.Fatalf("identity fence error=%v updates=%d", err, fixture.remote.updates)
	}
	after, err := fixture.store.GetRef(ctx, fixture.repoID, domain.RefBranch, "main")
	if err != nil || before != after {
		t.Fatalf("local pointer changed: %v", err)
	}
	err = fixture.service.AppendBranch(ctx, inbound.SyncInput{RepoID: fixture.repoID, ExpectedBranchID: domain.LegacyContextBranchID(fixture.repoID, "main")}, "main", fixture.target)
	if err != nil || fixture.remote.updates != 1 {
		t.Fatalf("valid identity rejected: %v updates=%d", err, fixture.remote.updates)
	}
}

func TestAppendBranchRejectsNaturalParentDisagreementBeforeLocalMutation(t *testing.T) {
	f := newAppendBranchFixture(t)
	ctx := context.Background()

	remoteBoundary := f.remote.snapshots[f.boundary]
	remoteBoundary.Parents = []domain.ContentHash{f.oldHead}
	remoteBoundary.GraftParents = nil
	remoteBoundary.Grafted = false
	f.remote.snapshots[f.boundary] = remoteBoundary
	remoteTarget := f.remote.snapshots[f.target]
	remoteTarget.GraftSeq = 7
	f.remote.snapshots[f.target] = remoteTarget

	err := f.service.AppendBranch(ctx, inbound.SyncInput{RepoID: f.repoID}, "main", f.target)
	if !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("AppendBranch error = %v, want ErrHashMismatch", err)
	}
	ref, gerr := f.store.GetRef(ctx, f.repoID, domain.RefBranch, "main")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if ref.Target != f.oldHead {
		t.Fatalf("local main moved after immutable metadata mismatch: %s", ref.Target)
	}
	target, gerr := f.store.GetSnapshot(ctx, f.target)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if target.GraftSeq != 0 {
		t.Fatalf("graft register mutated before full preflight: %+v", target)
	}
}

func TestAppendBranchRemoteReadFailureLeavesRefAndRetryConverges(t *testing.T) {
	f := newAppendBranchFixture(t)
	ctx := context.Background()
	f.remote.failGets[f.boundary] = 1

	err := f.service.AppendBranch(ctx, inbound.SyncInput{RepoID: f.repoID}, "main", f.target)
	if err == nil {
		t.Fatal("AppendBranch succeeded despite authoritative read failure")
	}
	ref, gerr := f.store.GetRef(ctx, f.repoID, domain.RefBranch, "main")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if ref.Target != f.oldHead {
		t.Fatalf("local main moved after read failure: %s", ref.Target)
	}

	if err := f.service.AppendBranch(ctx, inbound.SyncInput{RepoID: f.repoID}, "main", f.target); err != nil {
		t.Fatalf("retry did not converge: %v", err)
	}
	ref, gerr = f.store.GetRef(ctx, f.repoID, domain.RefBranch, "main")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if ref.Target != f.target || f.remote.updates != 2 {
		t.Fatalf("retry state = ref:%s updates:%d", ref.Target, f.remote.updates)
	}
}

func TestAppendBranchPreservesGenuinelyAheadLocalRef(t *testing.T) {
	f := newAppendBranchFixture(t)
	ctx := context.Background()
	localAhead := domain.HashContent([]byte("append-local-ahead"))
	if err := f.store.PutSnapshot(ctx, domain.Snapshot{
		ID: localAhead, RepoID: f.repoID, Branch: "main", DocHash: localAhead,
		Parents: []domain.ContentHash{f.oldHead},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutRef(ctx, domain.Ref{
		Kind: domain.RefBranch, Name: "main", RepoID: f.repoID, Target: localAhead,
	}); err != nil {
		t.Fatal(err)
	}

	if err := f.service.AppendBranch(ctx, inbound.SyncInput{RepoID: f.repoID}, "main", f.target); err != nil {
		t.Fatal(err)
	}
	ref, err := f.store.GetRef(ctx, f.repoID, domain.RefBranch, "main")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Target != localAhead {
		t.Fatalf("genuinely ahead local history was overwritten: %s", ref.Target)
	}
	boundary, err := f.store.GetSnapshot(ctx, f.boundary)
	if err != nil {
		t.Fatal(err)
	}
	if len(boundary.GraftParents) != 0 {
		t.Fatalf("unproven remote path partially reconciled: %+v", boundary.GraftParents)
	}
}

func TestReconcileAppendedPathIsBounded(t *testing.T) {
	repoID := string(domain.HashContent([]byte("bounded-append-repo")))
	ancestor := domain.HashContent([]byte("bounded-append-ancestor"))
	remote := &appendBranchRemote{
		snapshots: map[domain.ContentHash]domain.Snapshot{},
		failGets:  map[domain.ContentHash]int{},
	}
	ids := make([]domain.ContentHash, maxAppendReconcileSnapshots+1)
	for i := range ids {
		ids[i] = domain.HashContent([]byte(fmt.Sprintf("bounded-append-%d", i)))
	}
	for i, id := range ids {
		snap := domain.Snapshot{ID: id, RepoID: repoID, Branch: "feature", DocHash: id}
		if i+1 < len(ids) {
			snap.Parents = []domain.ContentHash{ids[i+1]}
		} else {
			snap.Grafted = true
			snap.GraftParents = []domain.ContentHash{ancestor}
			snap.GraftSeq = 1
		}
		remote.snapshots[id] = snap
	}

	svc := newTestSyncService(storage.NewFileStore(t.TempDir()), remote, nil)
	reached, err := svc.reconcileAppendedPath(context.Background(), repoID, ids[0], ancestor)
	if reached || !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("bounded traversal = reached:%v err:%v", reached, err)
	}
	if len(remote.getCalls) != maxAppendReconcileSnapshots {
		t.Fatalf("remote reads = %d, want %d", len(remote.getCalls), maxAppendReconcileSnapshots)
	}
}

func TestCompletedAppendReplayPreservesVerifiedDescendantWithoutRemoteWalk(t *testing.T) {
	ctx := context.Background()
	f := newAppendBranchFixture(t)
	// Replaying an old, completed PR returns its historical target. A published
	// graph larger than the hook budget must not make that harmless replay block
	// discovery of subsequent PRs when local ancestry already proves inclusion.
	older := domain.ContentHash("")
	for i := 0; i < maxAppendReconcileSnapshots+2; i++ {
		id := domain.HashContent([]byte(fmt.Sprintf("completed-pr-history-%d", i)))
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: f.repoID, Branch: "main"}
		if older != "" {
			snap.Parents = []domain.ContentHash{older}
		}
		if err := f.store.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		f.remote.snapshots[id] = snap
		older = id
	}
	target := older
	head := domain.HashContent([]byte("already-past-completed-pr"))
	if err := f.store.PutSnapshot(ctx, domain.Snapshot{ID: head, DocHash: head, RepoID: f.repoID, Branch: "main", Parents: []domain.ContentHash{target}}); err != nil {
		t.Fatal(err)
	}
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: f.repoID, Target: head}
	if err := f.store.PutRef(ctx, ref); err != nil {
		t.Fatal(err)
	}
	ref.Target = target
	if err := f.service.convergeAppendedBranch(ctx, f.repoID, ref); err != nil {
		t.Fatalf("completed replay blocked: %v", err)
	}
	after, err := f.store.GetRef(ctx, f.repoID, domain.RefBranch, "main")
	if err != nil || after.Target != head {
		t.Fatalf("rewound local progress %+v %v", after, err)
	}
	if len(f.remote.getCalls) != 0 {
		t.Fatalf("unnecessary remote walk: %d", len(f.remote.getCalls))
	}
}
