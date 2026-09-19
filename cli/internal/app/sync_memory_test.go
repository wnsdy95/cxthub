package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func putMemoryObject(t *testing.T, ctx context.Context, st *storage.FileStore, digest domain.MemoryDigest) domain.ContentHash {
	t.Helper()
	hash, err := st.PutMemory(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

type causalPullRemote struct {
	outbound.RemoteSync
	snapshot    domain.Snapshot
	doc         domain.SessionDoc
	latest      domain.MemoryDigest
	objects     map[domain.ContentHash]domain.MemoryDigest
	objectReads []domain.ContentHash
}

func (r *causalPullRemote) Pull(context.Context, string, map[domain.ContentHash]domain.ContentHash, []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	return []domain.Snapshot{r.snapshot}, []domain.SessionDoc{r.doc}, []domain.Ref{{
		Kind: domain.RefBranch, Name: "main", RepoID: r.snapshot.RepoID, Target: r.snapshot.ID,
	}}, nil
}

func (r *causalPullRemote) PullMemory(context.Context, string, domain.ContentHash) (domain.MemoryDigest, error) {
	return r.latest, nil
}

func (r *causalPullRemote) PullMemoryObject(_ context.Context, _ string, hash domain.ContentHash) (domain.MemoryDigest, error) {
	r.objectReads = append(r.objectReads, hash)
	digest, ok := r.objects[hash]
	if !ok {
		return domain.MemoryDigest{}, domain.ErrNotFound
	}
	return digest, nil
}

func TestPullFastForwardsRemoteMemoryDescendant(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("pull-memory-fast-forward-repo")))
	doc := pullDoc(t, "causal pull")
	st := storage.NewFileStore(t.TempDir())
	if _, err := st.PutDoc(ctx, doc); err != nil {
		t.Fatal(err)
	}
	root := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "root"}
	rootHash := putMemoryObject(t, ctx, st, root)
	middle := domain.MemoryDigest{SnapshotID: doc.Hash, PreviousMemoryHash: rootHash, Summary: "middle"}
	middleHash, err := domain.MemoryDigestHash(middle)
	if err != nil {
		t.Fatal(err)
	}
	latest := domain.MemoryDigest{SnapshotID: doc.Hash, PreviousMemoryHash: middleHash, Summary: "latest"}
	latestHash, err := domain.MemoryDigestHash(latest)
	if err != nil {
		t.Fatal(err)
	}
	local := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main", MemoryHash: rootHash}
	if err := st.PutSnapshot(ctx, local); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: doc.Hash}); err != nil {
		t.Fatal(err)
	}
	remoteSnapshot := local
	remoteSnapshot.MemoryHash = latestHash
	remote := &causalPullRemote{
		snapshot: remoteSnapshot, doc: doc, latest: latest,
		objects: map[domain.ContentHash]domain.MemoryDigest{middleHash: middle},
	}
	if _, err := newTestSyncService(st, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSnapshot(ctx, doc.Hash)
	if err != nil || got.MemoryHash != latestHash {
		t.Fatalf("memory pointer=%s want=%s err=%v", got.MemoryHash, latestHash, err)
	}
	if len(remote.objectReads) != 1 || remote.objectReads[0] != middleHash {
		t.Fatalf("remote ancestor reads=%v, want only middle", remote.objectReads)
	}
	if _, err := st.GetMemory(ctx, middleHash); err != nil {
		t.Fatalf("middle object was not staged: %v", err)
	}
}

func TestPullKeepsLocalMemoryDescendant(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("pull-memory-local-ahead-repo")))
	doc := pullDoc(t, "local causal pull")
	st := storage.NewFileStore(t.TempDir())
	if _, err := st.PutDoc(ctx, doc); err != nil {
		t.Fatal(err)
	}
	root := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "root"}
	rootHash := putMemoryObject(t, ctx, st, root)
	localTip := domain.MemoryDigest{SnapshotID: doc.Hash, PreviousMemoryHash: rootHash, Summary: "local tip"}
	localTipHash := putMemoryObject(t, ctx, st, localTip)
	local := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main", MemoryHash: localTipHash}
	if err := st.PutSnapshot(ctx, local); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: doc.Hash}); err != nil {
		t.Fatal(err)
	}
	remoteSnapshot := local
	remoteSnapshot.MemoryHash = rootHash
	remote := &causalPullRemote{snapshot: remoteSnapshot, doc: doc, latest: root, objects: map[domain.ContentHash]domain.MemoryDigest{}}
	if _, err := newTestSyncService(st, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSnapshot(ctx, doc.Hash)
	if err != nil || got.MemoryHash != localTipHash {
		t.Fatalf("local descendant was replaced: pointer=%s err=%v", got.MemoryHash, err)
	}
}

func TestPullRepairsMissingAncestorBehindCurrentMemoryPointer(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("pull-memory-repair-incomplete-chain-repo")))
	doc := pullDoc(t, "repair incomplete causal pull")
	st := storage.NewFileStore(t.TempDir())
	if _, err := st.PutDoc(ctx, doc); err != nil {
		t.Fatal(err)
	}
	root := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "remote root"}
	rootHash, err := domain.MemoryDigestHash(root)
	if err != nil {
		t.Fatal(err)
	}
	tip := domain.MemoryDigest{SnapshotID: doc.Hash, PreviousMemoryHash: rootHash, Summary: "shared tip"}
	tipHash := putMemoryObject(t, ctx, st, tip) // Deliberately omit root locally.
	snapshot := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main", MemoryHash: tipHash}
	if err := st.PutSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: doc.Hash}); err != nil {
		t.Fatal(err)
	}
	remote := &causalPullRemote{
		snapshot: snapshot,
		doc:      doc,
		latest:   tip,
		objects:  map[domain.ContentHash]domain.MemoryDigest{rootHash: root},
	}
	if _, err := newTestSyncService(st, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMemory(ctx, rootHash); err != nil {
		t.Fatalf("missing causal parent was not repaired: %v", err)
	}
	if len(remote.objectReads) != 1 || remote.objectReads[0] != rootHash {
		t.Fatalf("remote ancestor reads=%v, want only %s", remote.objectReads, rootHash)
	}
	got, err := st.GetSnapshot(ctx, doc.Hash)
	if err != nil || got.MemoryHash != tipHash {
		t.Fatalf("memory pointer=%s want=%s err=%v", got.MemoryHash, tipHash, err)
	}
}

type memoryStatusError int

func (e memoryStatusError) Error() string   { return "memory attachment conflict" }
func (e memoryStatusError) StatusCode() int { return int(e) }

type causalPushRemote struct {
	outbound.RemoteSync
	current   domain.ContentHash
	objects   map[domain.ContentHash]domain.MemoryDigest
	pushes    []domain.ContentHash
	refPushes int
}

type countingMemoryStore struct {
	outbound.SessionStore
	memoryReads int
}

func (s *countingMemoryStore) GetMemory(ctx context.Context, hash domain.ContentHash) (domain.MemoryDigest, error) {
	s.memoryReads++
	return s.SessionStore.GetMemory(ctx, hash)
}

func (r *causalPushRemote) Push(_ context.Context, _ string, _ []domain.Snapshot, _ []domain.SessionDoc, refs []domain.Ref, _ bool, _ bool) error {
	if len(refs) > 0 {
		r.refPushes++
	}
	return nil
}

func (r *causalPushRemote) RemoteManifest(_ context.Context, repoID string) (domain.Manifest, error) {
	attachments := map[domain.ContentHash]domain.ContentHash{}
	if r.current != "" {
		for _, digest := range r.objects {
			if hash, err := domain.MemoryDigestHash(digest); err == nil && hash == r.current {
				attachments[digest.SnapshotID] = r.current
				break
			}
		}
	}
	return domain.Manifest{RepoID: repoID, MemoryAttachments: attachments}, nil
}

func (r *causalPushRemote) PushMemory(_ context.Context, _ string, digest domain.MemoryDigest) error {
	hash, err := domain.MemoryDigestHash(digest)
	if err != nil {
		return err
	}
	r.pushes = append(r.pushes, hash)
	if r.current == hash {
		return nil
	}
	if r.current != digest.PreviousMemoryHash {
		return memoryStatusError(409)
	}
	r.objects[hash] = digest
	r.current = hash
	return nil
}

func (r *causalPushRemote) PullMemory(context.Context, string, domain.ContentHash) (domain.MemoryDigest, error) {
	if r.current == "" {
		return domain.MemoryDigest{}, domain.ErrNotFound
	}
	return r.objects[r.current], nil
}

func (r *causalPushRemote) PullMemoryObject(_ context.Context, _ string, hash domain.ContentHash) (domain.MemoryDigest, error) {
	digest, ok := r.objects[hash]
	if !ok {
		return domain.MemoryDigest{}, domain.ErrNotFound
	}
	return digest, nil
}

func (r *causalPushRemote) DeleteUnsyncRemote(context.Context, string, string) error { return nil }

func TestPushReplaysOnlyMissingMemorySuffix(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("push-memory-chain-repo")))
	doc := pullDoc(t, "causal push")
	st := storage.NewFileStore(t.TempDir())
	if _, err := st.PutDoc(ctx, doc); err != nil {
		t.Fatal(err)
	}
	root := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "root"}
	rootHash := putMemoryObject(t, ctx, st, root)
	middle := domain.MemoryDigest{SnapshotID: doc.Hash, PreviousMemoryHash: rootHash, Summary: "middle"}
	middleHash := putMemoryObject(t, ctx, st, middle)
	latest := domain.MemoryDigest{SnapshotID: doc.Hash, PreviousMemoryHash: middleHash, Summary: "latest"}
	latestHash := putMemoryObject(t, ctx, st, latest)
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main", MemoryHash: latestHash}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: doc.Hash}); err != nil {
		t.Fatal(err)
	}
	remote := &causalPushRemote{current: rootHash, objects: map[domain.ContentHash]domain.MemoryDigest{rootHash: root}}
	if _, err := newTestSyncService(st, remote, nil).Push(ctx, inbound.SyncInput{RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	want := []domain.ContentHash{middleHash, latestHash}
	if len(remote.pushes) != len(want) {
		t.Fatalf("push order=%v want=%v", remote.pushes, want)
	}
	for i := range want {
		if remote.pushes[i] != want[i] {
			t.Fatalf("push order=%v want=%v", remote.pushes, want)
		}
	}
	if remote.current != latestHash {
		t.Fatalf("remote pointer=%s want=%s", remote.current, latestHash)
	}
	remote.pushes = nil
	counting := &countingMemoryStore{SessionStore: st}
	if _, err := newTestSyncService(counting, remote, nil).Push(ctx, inbound.SyncInput{RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	if len(remote.pushes) != 0 {
		t.Fatalf("already-current memory was uploaded again: %v", remote.pushes)
	}
	if counting.memoryReads != 0 {
		t.Fatalf("already-current memory chain was reconstructed %d times", counting.memoryReads)
	}
}

func TestPushRejectsDivergentRemoteMemory(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("push-memory-divergence-repo")))
	doc := pullDoc(t, "divergent push")
	st := storage.NewFileStore(t.TempDir())
	if _, err := st.PutDoc(ctx, doc); err != nil {
		t.Fatal(err)
	}
	localRoot := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "local root"}
	localRootHash := putMemoryObject(t, ctx, st, localRoot)
	localTip := domain.MemoryDigest{SnapshotID: doc.Hash, PreviousMemoryHash: localRootHash, Summary: "local tip"}
	localTipHash := putMemoryObject(t, ctx, st, localTip)
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main", MemoryHash: localTipHash}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: doc.Hash}); err != nil {
		t.Fatal(err)
	}
	remoteRoot := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "remote root"}
	remoteRootHash, err := domain.MemoryDigestHash(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	remote := &causalPushRemote{current: remoteRootHash, objects: map[domain.ContentHash]domain.MemoryDigest{remoteRootHash: remoteRoot}}
	_, err = newTestSyncService(st, remote, nil).Push(ctx, inbound.SyncInput{RepoID: repo})
	if !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("push error=%v, want sync conflict", err)
	}
	if remote.current != remoteRootHash {
		t.Fatalf("divergent push moved remote pointer to %s", remote.current)
	}
	if remote.refPushes != 0 {
		t.Fatalf("divergent preflight published refs %d times", remote.refPushes)
	}
}

func TestPushSkipsRemoteMemoryDescendantAndPublishesRefs(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("push-memory-remote-ahead-repo")))
	doc := pullDoc(t, "remote-ahead causal push")
	st := storage.NewFileStore(t.TempDir())
	if _, err := st.PutDoc(ctx, doc); err != nil {
		t.Fatal(err)
	}
	root := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "root"}
	rootHash := putMemoryObject(t, ctx, st, root)
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main", MemoryHash: rootHash}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: doc.Hash}); err != nil {
		t.Fatal(err)
	}
	remoteTip := domain.MemoryDigest{SnapshotID: doc.Hash, PreviousMemoryHash: rootHash, Summary: "remote tip"}
	remoteTipHash, err := domain.MemoryDigestHash(remoteTip)
	if err != nil {
		t.Fatal(err)
	}
	remote := &causalPushRemote{
		current: remoteTipHash,
		objects: map[domain.ContentHash]domain.MemoryDigest{rootHash: root, remoteTipHash: remoteTip},
	}
	if _, err := newTestSyncService(st, remote, nil).Push(ctx, inbound.SyncInput{RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	if len(remote.pushes) != 0 {
		t.Fatalf("stale local memory attempted remote rewind: %v", remote.pushes)
	}
	if remote.current != remoteTipHash {
		t.Fatalf("remote memory moved to %s, want %s", remote.current, remoteTipHash)
	}
	if remote.refPushes != 1 {
		t.Fatalf("refs published %d times, want 1", remote.refPushes)
	}
}

// The catalog and CAS are separate requests. Another uploader can legitimately
// publish a later generation between them; that does not prove a memory fork.
type advancingMemoryRemote struct {
	*causalPushRemote
	advance domain.ContentHash
	once    bool
}

func (r *advancingMemoryRemote) PushMemory(ctx context.Context, repo string, d domain.MemoryDigest) error {
	if !r.once {
		r.once = true
		r.current = r.advance
	}
	return r.causalPushRemote.PushMemory(ctx, repo, d)
}

func TestPushMemoryReconcilesConcurrentSameChain(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "optimistic", true: "catalog"}[known], func(t *testing.T) {
			ctx := context.Background()
			st := storage.NewFileStore(t.TempDir())
			snapshot := domain.HashContent([]byte("same chain"))
			root := domain.MemoryDigest{SnapshotID: snapshot, Summary: "root"}
			rh := putMemoryObject(t, ctx, st, root)
			mid := domain.MemoryDigest{SnapshotID: snapshot, PreviousMemoryHash: rh, Summary: "middle"}
			mh := putMemoryObject(t, ctx, st, mid)
			tip := domain.MemoryDigest{SnapshotID: snapshot, PreviousMemoryHash: mh, Summary: "tip"}
			th := putMemoryObject(t, ctx, st, tip)
			remote := &advancingMemoryRemote{causalPushRemote: &causalPushRemote{objects: map[domain.ContentHash]domain.MemoryDigest{rh: root, mh: mid}}, advance: mh}
			svc := newTestSyncService(st, remote, nil)
			plan, err := svc.localMemoryPushPlan(ctx, snapshot, th)
			if err != nil {
				t.Fatal(err)
			}
			if known {
				err = svc.pushMemoryPlanFromKnown(ctx, "repo", plan, "")
			} else {
				err = svc.pushMemoryPlan(ctx, "repo", plan)
			}
			if err != nil {
				t.Fatalf("same-chain concurrent progress reported as fork: %v", err)
			}
			if remote.current != th {
				t.Fatal("remaining suffix was not published")
			}
		})
	}
}

func TestPushMemoryPreservesConcurrentRemoteDescendant(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	snapshot := domain.HashContent([]byte("remote ahead"))
	root := domain.MemoryDigest{SnapshotID: snapshot, Summary: "root"}
	rh := putMemoryObject(t, ctx, st, root)
	tip := domain.MemoryDigest{SnapshotID: snapshot, PreviousMemoryHash: rh, Summary: "tip"}
	th := putMemoryObject(t, ctx, st, tip)
	newer := domain.MemoryDigest{SnapshotID: snapshot, PreviousMemoryHash: th, Summary: "new server memory"}
	nh, _ := domain.MemoryDigestHash(newer)
	remote := &advancingMemoryRemote{causalPushRemote: &causalPushRemote{current: rh, objects: map[domain.ContentHash]domain.MemoryDigest{rh: root, th: tip, nh: newer}}, advance: nh}
	svc := newTestSyncService(st, remote, nil)
	plan, err := svc.localMemoryPushPlan(ctx, snapshot, th)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.pushMemoryPlanFromKnown(ctx, "repo", plan, rh); err != nil {
		t.Fatalf("proven remote descendant rejected: %v", err)
	}
	if remote.current != nh {
		t.Fatal("remote memory was rewound")
	}
	if _, err := st.GetMemory(ctx, nh); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("push silently adopted remote memory")
	}
}

type contendedMemoryRemote struct {
	*causalPushRemote
	calls   int
	failure error
}

func (r *contendedMemoryRemote) PushMemory(context.Context, string, domain.MemoryDigest) error {
	r.calls++
	return r.failure
}

func TestPushMemoryContentionIsBoundedAndNotAFork(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	snapshot := domain.HashContent([]byte("contention"))
	root := domain.MemoryDigest{SnapshotID: snapshot, Summary: "root"}
	rh := putMemoryObject(t, ctx, st, root)
	tip := domain.MemoryDigest{SnapshotID: snapshot, PreviousMemoryHash: rh, Summary: "tip"}
	th := putMemoryObject(t, ctx, st, tip)
	for _, failure := range []error{memoryStatusError(409), memoryStatusError(503), context.Canceled} {
		remote := &contendedMemoryRemote{causalPushRemote: &causalPushRemote{current: rh, objects: map[domain.ContentHash]domain.MemoryDigest{rh: root}}, failure: failure}
		svc := newTestSyncService(st, remote, nil)
		plan, err := svc.localMemoryPushPlan(ctx, snapshot, th)
		if err != nil {
			t.Fatal(err)
		}
		err = svc.pushMemoryPlanFromKnown(ctx, "repo", plan, rh)
		if isMemoryAttachmentConflict(failure) {
			if !errors.Is(err, domain.ErrMemoryContention) || errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("contention classified as fork: %v", err)
			}
			if remote.calls != maxMemoryPushReplans+1 {
				t.Fatalf("unbounded retries: %d", remote.calls)
			}
		} else if !errors.Is(err, failure) || remote.calls != 1 {
			t.Fatalf("non-CAS error retried: %v calls=%d", err, remote.calls)
		}
	}
}

func TestPushMemoryConcurrentForkStopsBeforeRefs(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	doc := pullDoc(t, "concurrent fork")
	repo := string(domain.HashContent([]byte("fork repo")))
	if _, err := st.PutDoc(ctx, doc); err != nil {
		t.Fatal(err)
	}
	root := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "root"}
	rh := putMemoryObject(t, ctx, st, root)
	tip := domain.MemoryDigest{SnapshotID: doc.Hash, PreviousMemoryHash: rh, Summary: "local"}
	th := putMemoryObject(t, ctx, st, tip)
	sibling := domain.MemoryDigest{SnapshotID: doc.Hash, PreviousMemoryHash: rh, Summary: "remote sibling"}
	sh, _ := domain.MemoryDigestHash(sibling)
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main", MemoryHash: th}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: doc.Hash}); err != nil {
		t.Fatal(err)
	}
	remote := &advancingMemoryRemote{causalPushRemote: &causalPushRemote{current: rh, objects: map[domain.ContentHash]domain.MemoryDigest{rh: root, sh: sibling}}, advance: sh}
	_, err := newTestSyncService(st, remote, nil).Push(ctx, inbound.SyncInput{RepoID: repo})
	if !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("fork accepted: %v", err)
	}
	if remote.current != sh || remote.refPushes != 0 {
		t.Fatal("fork rewrote remote memory or published refs")
	}
}

func TestMemoryPreflightUsesActualCurrentDigest(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	snapshot := domain.HashContent([]byte("preflight"))
	root := domain.MemoryDigest{SnapshotID: snapshot, Summary: "root"}
	rh := putMemoryObject(t, ctx, st, root)
	newer := domain.MemoryDigest{SnapshotID: snapshot, PreviousMemoryHash: rh, Summary: "remote"}
	nh, _ := domain.MemoryDigestHash(newer)
	latest := domain.MemoryDigest{SnapshotID: snapshot, PreviousMemoryHash: nh, Summary: "newer remote"}
	lh, _ := domain.MemoryDigestHash(latest)
	remote := &causalPushRemote{current: lh, objects: map[domain.ContentHash]domain.MemoryDigest{nh: newer, lh: latest}}
	svc := newTestSyncService(st, remote, nil)
	plan, err := svc.localMemoryPushPlan(ctx, snapshot, rh)
	if err != nil {
		t.Fatal(err)
	}
	ahead, err := svc.preflightKnownRemoteMemory(ctx, "repo", plan, nh)
	if err != nil || !ahead {
		t.Fatalf("catalog race rejected verified descendant: ahead=%v err=%v", ahead, err)
	}
	// The returned envelope must be for this exact snapshot even during a race.
	latest.SnapshotID = domain.HashContent([]byte("foreign"))
	remote.objects[lh] = latest
	if _, err := svc.preflightKnownRemoteMemory(ctx, "repo", plan, nh); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("foreign memory accepted: %v", err)
	}
}
