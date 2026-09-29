package app

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestFetchObservesMemoryAndGraftWithoutApplyingThenPullApplies(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := string(domain.HashContent([]byte("fetch observation repo")))
	store := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", strings.Repeat("a", 40))
	doc, parentDoc := pullDoc(t, "current"), pullDoc(t, "joined")
	for _, d := range []domain.SessionDoc{doc, parentDoc} {
		if _, err := store.PutDoc(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	parent := domain.Snapshot{ID: parentDoc.Hash, DocHash: parentDoc.Hash, RepoID: repo}
	if err := store.PutSnapshot(ctx, parent); err != nil {
		t.Fatal(err)
	}
	memory := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "original memory"}
	oldMemory := putMemoryObject(t, ctx, store, memory)
	latest := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "new memory", PreviousMemoryHash: oldMemory}
	latestHash, err := domain.MemoryDigestHash(latest)
	if err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main", MemoryHash: oldMemory}
	if err := store.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: doc.Hash}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: repo, Branch: "main", GitCommit: strings.Repeat("a", 40), Snapshot: doc.Hash, MemoryHash: oldMemory, SharedTarget: doc.Hash}); err != nil {
		t.Fatal(err)
	}
	before, err := store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remoteSnap := snap
	remoteSnap.MemoryHash = latestHash
	remoteSnap.GraftParents = []domain.ContentHash{parent.ID}
	remoteSnap.GraftSeq = 1
	remoteSnap.Grafted = true
	remote := &causalPullRemote{snapshot: remoteSnap, doc: doc, latest: latest, objects: map[domain.ContentHash]domain.MemoryDigest{oldMemory: memory}}
	delta := &deltaCausalRemote{causalPullRemote: remote}
	svc := newTestSyncService(store, delta, nil)
	if _, err := svc.Pull(ctx, inbound.SyncInput{RepoID: repo, FetchOnly: true}); err != nil {
		t.Fatal(err)
	}
	after, err := store.GetSnapshot(ctx, doc.Hash)
	if err != nil || !reflect.DeepEqual(after, snap) {
		t.Fatalf("fetch applied mutable metadata: %+v %v", after, err)
	}
	position, err := store.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(position, before) {
		t.Fatalf("fetch moved selection: %+v %v", position, err)
	}
	observation, err := store.ReadRemoteObservation(ctx, repo, "configured")
	if err != nil || len(observation.Snapshots) != 1 || observation.Snapshots[0].MemoryHash != latestHash {
		t.Fatalf("remote update was not durably observed: %+v %v", observation, err)
	}
	if _, err := svc.Pull(ctx, inbound.SyncInput{RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	if delta.omitted != 1 {
		t.Fatalf("pull should use the verified fetch observation, delta omissions=%d", delta.omitted)
	}
	after, err = store.GetSnapshot(ctx, doc.Hash)
	if err != nil || after.MemoryHash != latestHash || len(after.GraftParents) != 1 {
		t.Fatalf("pull did not apply observation: %+v %v", after, err)
	}
	// Application changes the shared snapshot attachment; this explicitly pinned
	// worktree memory is a separate code selection and remains stable.
	position, err = store.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(position, before) {
		t.Fatalf("pull repinned worktree implicitly: %+v %v", position, err)
	}
}

func TestPullLocalAheadIsNotConflict(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("local ahead repo")))
	store := storage.NewFileStore(t.TempDir())
	rootDoc, tipDoc := pullDoc(t, "root"), pullDoc(t, "local tip")
	for _, d := range []domain.SessionDoc{rootDoc, tipDoc} {
		if _, err := store.PutDoc(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	root := domain.Snapshot{ID: rootDoc.Hash, DocHash: rootDoc.Hash, RepoID: repo, Branch: "main"}
	tip := domain.Snapshot{ID: tipDoc.Hash, DocHash: tipDoc.Hash, RepoID: repo, Branch: "main", Parents: []domain.ContentHash{root.ID}}
	for _, s := range []domain.Snapshot{root, tip} {
		if err := store.PutSnapshot(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: tip.ID}); err != nil {
		t.Fatal(err)
	}
	remote := &inventoryPullRemote{refs: []domain.Ref{{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: root.ID}}}
	out, err := newTestSyncService(store, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repo})
	if err != nil || len(out.Conflicts) > 0 {
		t.Fatalf("local ahead falsely conflicted: %+v %v", out, err)
	}
	ref, err := store.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || ref.Target != tip.ID {
		t.Fatalf("local ahead rewound: %+v %v", ref, err)
	}
}

// deltaCausalRemote models the exact fetch-to-pull negotiation: after fetch
// records the remote token, pull receives no snapshot metadata at all.
type deltaCausalRemote struct {
	*causalPullRemote
	omitted int
}

func (r *deltaCausalRemote) Pull(ctx context.Context, repo string, states map[domain.ContentHash]domain.ContentHash, docs []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	state, err := domain.SnapshotStateHash(r.snapshot)
	if err != nil {
		return nil, nil, nil, err
	}
	if states[r.snapshot.ID] == state {
		r.omitted++
		return nil, nil, []domain.Ref{{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: r.snapshot.ID}}, nil
	}
	return r.causalPullRemote.Pull(ctx, repo, states, docs)
}

func TestFetchCursorAndObservationAreScopedToResolvedEndpoint(t *testing.T) {
	f := newRemoteStateCursorFixture(t)
	first := f.svc.WithRemoteIdentity("https://first.example/api/v1")
	for i, want := range []int{1, 0} {
		got, err := first.Pull(f.ctx, inbound.SyncInput{RepoID: f.repoID, FetchOnly: true})
		if err != nil || got.Pulled != want {
			t.Fatalf("first endpoint pull %d=%+v %v", i, got, err)
		}
	}
	second := f.svc.WithRemoteIdentity("https://second.example/api/v1")
	got, err := second.Pull(f.ctx, inbound.SyncInput{RepoID: f.repoID, FetchOnly: true})
	if err != nil || got.Pulled != 1 {
		t.Fatalf("another endpoint reused the first cursor: %+v %v", got, err)
	}
	for _, endpoint := range []string{"https://first.example/api/v1", "https://second.example/api/v1"} {
		observed, err := f.store.ReadRemoteObservation(f.ctx, f.repoID, endpoint)
		if err != nil || observed.Remote != endpoint || observed.Revision == "" {
			t.Fatalf("missing endpoint observation %+v %v", observed, err)
		}
	}
}
