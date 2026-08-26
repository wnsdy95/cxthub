package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type projectionMutationStore struct {
	*storage.FileStore
	target, graft domain.ContentHash
	remaining     int // -1 mutates on every successful memory read.
}

func (s *projectionMutationStore) GetMemory(ctx context.Context, hash domain.ContentHash) (domain.MemoryDigest, error) {
	digest, err := s.FileStore.GetMemory(ctx, hash)
	if err == nil && s.remaining != 0 {
		if s.remaining > 0 {
			s.remaining--
		}
		s.mutateGraft(ctx)
	}
	return digest, err
}

func (s *projectionMutationStore) mutateGraft(ctx context.Context) {
	snap, err := s.FileStore.GetSnapshot(ctx, s.target)
	if err != nil {
		return
	}
	snap.GraftSeq++
	if len(snap.GraftParents) == 0 {
		snap.Grafted = true
		snap.GraftParents = []domain.ContentHash{s.graft}
	} else {
		snap.Grafted = false
		snap.GraftParents = nil
	}
	_ = s.FileStore.PutSnapshot(ctx, snap)
}

type graftBeforeMemoryAttachStore struct {
	*storage.FileStore
	target, graft domain.ContentHash
	mutated       bool
}

type missingSelectedMemoryStore struct {
	*storage.FileStore
	missing domain.ContentHash
}

func (s *missingSelectedMemoryStore) GetMemory(ctx context.Context, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if hash == s.missing {
		return domain.MemoryDigest{}, domain.ErrNotFound
	}
	return s.FileStore.GetMemory(ctx, hash)
}

func (s *graftBeforeMemoryAttachStore) PutMemory(ctx context.Context, digest domain.MemoryDigest) (domain.ContentHash, error) {
	hash, err := s.FileStore.PutMemory(ctx, digest)
	if err == nil && !s.mutated {
		s.mutated = true
		mutator := projectionMutationStore{FileStore: s.FileStore, target: s.target, graft: s.graft}
		mutator.mutateGraft(ctx)
	}
	return hash, err
}

// TestMergeDigests fixes the determinism rules for memory digest merges:
// ancestor precedence + dedup, disallow duplicate links if summaries are contained.
func TestMergeDigests(t *testing.T) {
	prior := domain.MemoryDigest{Summary: "Previous session summary", KeyFacts: []string{"A", "B"}, OpenTasks: []string{"t1"}}
	fresh := domain.MemoryDigest{Summary: "New session summary", KeyFacts: []string{"B", "C"}, OpenTasks: []string{"t1", "t2"}}
	m := domain.MergeDigests(prior, fresh)
	if !strings.Contains(m.Summary, "Previous session summary") || !strings.Contains(m.Summary, "New session summary") {
		t.Fatalf("Summary merge failure: %q", m.Summary)
	}
	if got := strings.Join(m.KeyFacts, ","); got != "A,B,C" {
		t.Fatalf("key_facts merge: %s", got)
	}
	if got := strings.Join(m.OpenTasks, ","); got != "t1,t2" {
		t.Fatalf("open_tasks merge: %s", got)
	}
	// Avoid double linking summaries if fresh contains prior summary.
	same := domain.MergeDigests(prior, domain.MemoryDigest{Summary: "previous session summary + addition"})
	if strings.Count(same.Summary, "previous session summary") != 1 {
		t.Fatalf("inclusion summary has duplicate links: %q", same.Summary)
	}
}

// TestNearestAncestorDigest fixes reachability parent chain BFS traversal: skips memory-less intermediate
// snapshots and finds the nearest ancestor digest.
func TestNearestAncestorDigest(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	// A(with memory) ← B(without) ← C. When searching from C, the digest of A should be returned.
	memHash, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: h('a'), Summary: "ancestor memory", KeyFacts: []string{"fact-a"}})
	if err != nil {
		t.Fatal(err)
	}
	put := func(id domain.ContentHash, mem domain.ContentHash, parents ...domain.ContentHash) {
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, Branch: "main", Parents: parents, MemoryHash: mem}); err != nil {
			t.Fatal(err)
		}
	}
	put(h('a'), memHash)
	put(h('b'), "", h('a'))
	put(h('c'), "", h('b'))

	c, _ := st.GetSnapshot(ctx, h('c'))
	d, ok := nearestAncestorDigest(ctx, st, c)
	if !ok || d.Summary != "ancestor memory" {
		t.Fatalf("inheritance failed: ok=%v summary=%q", ok, d.Summary)
	}
	// Root (no parent) has no successor.
	a, _ := st.GetSnapshot(ctx, h('a'))
	if _, ok := nearestAncestorDigest(ctx, st, a); ok {
		t.Fatal("Successor occurred at root")
	}

	// Immutable overlay graft: D has no natural parent and A is only connected via GraftParents.
	// Starting from E, A's memory must be found even after passing through memoryless D.
	if err := st.PutSnapshot(ctx, domain.Snapshot{
		ID: h('d'), DocHash: h('d'), Branch: "main", Grafted: true,
		GraftParents: []domain.ContentHash{h('a')},
	}); err != nil {
		t.Fatal(err)
	}
	put(h('e'), "", h('d'))
	e, _ := st.GetSnapshot(ctx, h('e'))
	d, ok = nearestAncestorDigest(ctx, st, e)
	if !ok || d.Summary != "ancestor memory" {
		t.Fatalf("overlay graft inheritance failed: ok=%v summary=%q", ok, d.Summary)
	}
}

func TestAncestorMemoryProjectionUnionsParallelGraftFragments(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	base := domain.MemoryDigest{SnapshotID: h('a'), Summary: "shared base"}
	left := domain.MergeDigests(base, domain.MemoryDigest{SnapshotID: h('b'), Summary: "left PR"})
	right := domain.MergeDigests(base, domain.MemoryDigest{SnapshotID: h('c'), Summary: "right PR"})
	baseHash, err := st.PutMemory(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	leftHash, err := st.PutMemory(ctx, left)
	if err != nil {
		t.Fatal(err)
	}
	rightHash, err := st.PutMemory(ctx, right)
	if err != nil {
		t.Fatal(err)
	}
	for _, snap := range []domain.Snapshot{
		{ID: h('a'), DocHash: h('a'), Branch: "main", MemoryHash: baseHash},
		{ID: h('b'), DocHash: h('b'), Branch: "main", Parents: []domain.ContentHash{h('a')}, MemoryHash: leftHash},
		{ID: h('c'), DocHash: h('c'), Branch: "main", Parents: []domain.ContentHash{h('a')}, MemoryHash: rightHash},
		{ID: h('d'), DocHash: h('d'), Branch: "main", Parents: []domain.ContentHash{h('b')}, GraftParents: []domain.ContentHash{h('c')}, Grafted: true},
	} {
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	merge, _ := st.GetSnapshot(ctx, h('d'))
	got, ok := ancestorMemoryProjection(ctx, st, merge)
	if !ok {
		t.Fatal("parallel memory projection not found")
	}
	if strings.Count(got.Summary, "shared base") != 1 || !strings.Contains(got.Summary, "left PR") || !strings.Contains(got.Summary, "right PR") {
		t.Fatalf("parallel projection = %q", got.Summary)
	}
	if len(got.Fragments) != 3 {
		t.Fatalf("fragment union = %d, want shared+left+right", len(got.Fragments))
	}
}

// A server append can add a graft overlay after the boundary snapshot already
// has memory. That digest only covers the pre-graft lineage, so a child must
// not treat it as a complete projection of the boundary's current ancestry.
func TestAncestorMemoryProjectionDoesNotShadowGraftBehindMemory(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	putMemory := func(d domain.MemoryDigest) domain.ContentHash {
		hash, err := st.PutMemory(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	putSnapshot := func(s domain.Snapshot) {
		if err := st.PutSnapshot(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	base := domain.MemoryDigest{SnapshotID: h('a'), Summary: "shared base"}
	baseHash := putMemory(base)
	teammate := domain.MergeDigests(base, domain.MemoryDigest{SnapshotID: h('b'), Summary: "teammate work"})
	teammateHash := putMemory(teammate)
	// This is the digest created before the server attached T as a graft.
	// Legacy cumulative digest: no fragment provenance and no coverage marker.
	local := domain.MemoryDigest{SnapshotID: h('d'), Summary: "local work"}
	localHash := putMemory(local)
	descendant := domain.MemoryDigest{SnapshotID: h('e'), Summary: "local work\n\ndescendant work"}
	descendantHash := putMemory(descendant)

	putSnapshot(domain.Snapshot{ID: h('a'), DocHash: h('a'), Branch: "main", MemoryHash: baseHash})
	putSnapshot(domain.Snapshot{ID: h('b'), DocHash: h('b'), Branch: "main", Parents: []domain.ContentHash{h('a')}, MemoryHash: teammateHash})
	putSnapshot(domain.Snapshot{
		ID: h('d'), DocHash: h('d'), Branch: "main", Parents: []domain.ContentHash{h('a')},
		GraftParents: []domain.ContentHash{h('b')}, Grafted: true, GraftSeq: 1, MemoryHash: localHash,
	})
	putSnapshot(domain.Snapshot{
		ID: h('e'), DocHash: h('e'), Branch: "main", Parents: []domain.ContentHash{h('d')}, MemoryHash: descendantHash,
	})

	boundary, _ := st.GetSnapshot(ctx, h('d'))
	direct, ok := snapshotMemoryProjection(ctx, st, boundary)
	if !ok || !strings.Contains(direct.Summary, "local work") || !strings.Contains(direct.Summary, "teammate work") {
		t.Fatalf("direct stale boundary projection = %q, want local + graft memory", direct.Summary)
	}
	child, _ := st.GetSnapshot(ctx, h('e'))
	direct, ok = snapshotMemoryProjection(ctx, st, child)
	if !ok || !strings.Contains(direct.Summary, "descendant work") || !strings.Contains(direct.Summary, "teammate work") {
		t.Fatalf("legacy descendant projection = %q, want descendant + hidden graft memory", direct.Summary)
	}
	got, ok := ancestorMemoryProjection(ctx, st, child)
	if !ok || !strings.Contains(got.Summary, "local work") || !strings.Contains(got.Summary, "teammate work") {
		t.Fatalf("stale boundary projection = %q, want local + graft memory", got.Summary)
	}
}

// A memoryless boundary is also not a single lineage. Its child must inherit
// the nearest digest from every natural/graft parent instead of the first BFS
// winner only.
func TestAncestorMemoryProjectionUnionsThroughMemorylessBoundary(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	put := func(s domain.Snapshot, summary string) {
		if summary != "" {
			memoryHash, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: s.ID, Summary: summary})
			if err != nil {
				t.Fatal(err)
			}
			s.MemoryHash = memoryHash
		}
		if err := st.PutSnapshot(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	put(domain.Snapshot{ID: h('b'), DocHash: h('b'), Branch: "main"}, "natural lineage")
	put(domain.Snapshot{ID: h('c'), DocHash: h('c'), Branch: "main"}, "graft lineage")
	put(domain.Snapshot{
		ID: h('d'), DocHash: h('d'), Branch: "main", Parents: []domain.ContentHash{h('b')},
		GraftParents: []domain.ContentHash{h('c')}, Grafted: true, GraftSeq: 1,
	}, "")
	put(domain.Snapshot{ID: h('e'), DocHash: h('e'), Branch: "main", Parents: []domain.ContentHash{h('d')}}, "")

	child, _ := st.GetSnapshot(ctx, h('e'))
	got, ok := ancestorMemoryProjection(ctx, st, child)
	if !ok || !strings.Contains(got.Summary, "natural lineage") || !strings.Contains(got.Summary, "graft lineage") {
		t.Fatalf("memoryless boundary projection = %q, want both lineages", got.Summary)
	}
}

func TestMemoryProjectionStopsAtCurrentGraftCoverage(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	digest := domain.MergeDigests(
		domain.MemoryDigest{SnapshotID: h('b'), Summary: "covered teammate work"},
		domain.MemoryDigest{SnapshotID: h('d'), Summary: "covered local work"},
	)
	teammateHash, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: h('b'), Summary: "covered teammate work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: h('b'), DocHash: h('b'), Branch: "main", MemoryHash: teammateHash}); err != nil {
		t.Fatal(err)
	}
	boundary := domain.Snapshot{
		ID: h('d'), DocHash: h('d'), Branch: "main", Grafted: true, GraftSeq: 1,
		GraftParents: []domain.ContentHash{h('b')},
	}
	if err := st.PutSnapshot(ctx, boundary); err != nil {
		t.Fatal(err)
	}
	digest.GraftCoverage = memoryGraftCoverage(ctx, st, boundary, digest.Fragments, true)
	if digest.GraftCoverage == nil {
		t.Fatal("complete boundary did not produce coverage")
	}
	memoryHash, err := st.PutMemory(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	boundary.MemoryHash = memoryHash
	if err := st.PutSnapshot(ctx, boundary); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: h('e'), DocHash: h('e'), Parents: []domain.ContentHash{h('d')}}); err != nil {
		t.Fatal(err)
	}
	child, _ := st.GetSnapshot(ctx, h('e'))
	got, ok := ancestorMemoryProjection(ctx, st, child)
	if !ok || !strings.Contains(got.Summary, "covered teammate work") || !strings.Contains(got.Summary, "covered local work") {
		t.Fatalf("current coverage projection = %q", got.Summary)
	}
}

func TestPriorMemoryProjectionReplacesOwnContributionButKeepsPinnedImport(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	base := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: h('a'), Summary: "reachable prior"})
	baseHash, err := st.PutMemory(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	rootDigest := domain.MergeDigests(base, domain.MemoryDigest{SnapshotID: h('c'), Summary: "external pinned import"})
	rootDigest = domain.MergeDigests(rootDigest, domain.MemoryDigest{SnapshotID: h('d'), Summary: "old root contribution"})
	root := domain.Snapshot{ID: h('d'), DocHash: h('d'), Branch: "main", Parents: []domain.ContentHash{h('a')}}
	for _, snap := range []domain.Snapshot{
		{ID: h('a'), DocHash: h('a'), Branch: "main", MemoryHash: baseHash},
		root,
	} {
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	rootDigest.GraftCoverage = memoryGraftCoverage(ctx, st, root, rootDigest.Fragments, true)
	if rootDigest.GraftCoverage == nil || len(rootDigest.GraftCoverage.PinnedSources) != 1 || rootDigest.GraftCoverage.PinnedSources[0] != h('c') {
		t.Fatalf("root pinned coverage = %+v", rootDigest.GraftCoverage)
	}
	rootHash, err := st.PutMemory(ctx, rootDigest)
	if err != nil {
		t.Fatal(err)
	}
	root.MemoryHash = rootHash
	if err := st.PutSnapshot(ctx, root); err != nil {
		t.Fatal(err)
	}
	prior, ok := priorMemoryProjection(ctx, st, root)
	if !ok || !strings.Contains(prior.Summary, "reachable prior") || !strings.Contains(prior.Summary, "external pinned import") {
		t.Fatalf("same-snapshot prior projection = %q", prior.Summary)
	}
	if strings.Contains(prior.Summary, "old root contribution") {
		t.Fatalf("same-snapshot prior retained replaced own contribution: %q", prior.Summary)
	}
}

func TestStableMemoryProjectionRetriesConcurrentGraftMovement(t *testing.T) {
	ctx := context.Background()
	baseStore := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	putMemory := func(id domain.ContentHash, summary string) domain.ContentHash {
		hash, err := baseStore.PutMemory(ctx, domain.MemoryDigest{SnapshotID: id, Summary: summary})
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	for _, snap := range []domain.Snapshot{
		{ID: h('a'), DocHash: h('a'), Branch: "main", MemoryHash: putMemory(h('a'), "natural memory")},
		{ID: h('c'), DocHash: h('c'), Branch: "main", MemoryHash: putMemory(h('c'), "concurrent graft memory")},
		{ID: h('d'), DocHash: h('d'), Branch: "main", Parents: []domain.ContentHash{h('a')}},
	} {
		if err := baseStore.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	store := &projectionMutationStore{FileStore: baseStore, target: h('d'), graft: h('c'), remaining: 1}
	state, prior, found, complete, err := stablePriorMemoryProjection(ctx, store, h('d'))
	if err != nil {
		t.Fatal(err)
	}
	if !found || !complete || state.snap.GraftSeq != 1 || len(state.snap.GraftParents) != 1 || state.snap.GraftParents[0] != h('c') {
		t.Fatalf("stable retry state=%+v found=%v complete=%v", state.snap, found, complete)
	}
	if !strings.Contains(prior.Summary, "natural memory") || !strings.Contains(prior.Summary, "concurrent graft memory") {
		t.Fatalf("stable retry projected stale state: %q", prior.Summary)
	}

	store = &projectionMutationStore{FileStore: baseStore, target: h('d'), graft: h('c'), remaining: -1}
	if _, _, _, _, err := stablePriorMemoryProjection(ctx, store, h('d')); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("continuously moving lineage error = %v, want sync conflict", err)
	}
}

func TestMemorizeAttachmentDoesNotReplayConcurrentGraftRegister(t *testing.T) {
	ctx := context.Background()
	baseStore := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-memory-attach-race", DefaultBranch: "main", LocalPath: t.TempDir()}
	docHash, err := baseStore.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex},
		Events:   []domain.Event{{Kind: domain.EventMessage, Role: "user", Seq: 0}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	graftID := domain.HashContent([]byte("attachment-race-graft"))
	graftMemory, err := baseStore.PutMemory(ctx, domain.MemoryDigest{SnapshotID: graftID, Summary: "concurrent graft survives attachment"})
	if err != nil {
		t.Fatal(err)
	}
	for _, snap := range []domain.Snapshot{
		{ID: graftID, DocHash: graftID, RepoID: repo.ID, Branch: "main", MemoryHash: graftMemory},
		{ID: docHash, DocHash: docHash, RepoID: repo.ID, Branch: "main"},
	} {
		if err := baseStore.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	if err := baseStore.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo.ID, Target: docHash}); err != nil {
		t.Fatal(err)
	}
	store := &graftBeforeMemoryAttachStore{FileStore: baseStore, target: docHash, graft: graftID}
	service := NewMemorizeService(
		branchSeedGit{repo: repo}, nil, nil, nil,
		stubDistiller{d: domain.MemoryDigest{Summary: "fresh root memory"}}, store,
	)
	out, err := service.Memorize(ctx, inbound.MemorizeInput{Cwd: repo.LocalPath, Provider: domain.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	root, err := baseStore.GetSnapshot(ctx, docHash)
	if err != nil {
		t.Fatal(err)
	}
	if root.MemoryHash != out.MemoryHash || root.GraftSeq != 1 || len(root.GraftParents) != 1 || root.GraftParents[0] != graftID {
		t.Fatalf("memory attachment replayed stale graft state: %+v", root)
	}
	projected, ok := snapshotMemoryProjection(ctx, baseStore, root)
	if !ok || !strings.Contains(projected.Summary, "fresh root memory") || !strings.Contains(projected.Summary, "concurrent graft survives attachment") {
		t.Fatalf("post-race projection = %q", projected.Summary)
	}
}

func TestMemoryProjectionRebuildsSupersededGraftFromCurrentParents(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	base := domain.MemoryDigest{SnapshotID: h('a'), Summary: "current base"}
	baseHash, err := st.PutMemory(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	stale := domain.MergeDigests(base, domain.MemoryDigest{SnapshotID: h('b'), Summary: "removed graft work"})
	stale = domain.MergeDigests(stale, domain.MemoryDigest{SnapshotID: h('d'), Summary: "boundary own work"})
	removedHash, err := st.PutMemory(ctx, domain.MemoryDigest{SnapshotID: h('b'), Summary: "removed graft work"})
	if err != nil {
		t.Fatal(err)
	}
	boundary := domain.Snapshot{
		ID: h('d'), DocHash: h('d'), Branch: "main", Parents: []domain.ContentHash{h('a')},
		Grafted: true, GraftSeq: 1, GraftParents: []domain.ContentHash{h('b')},
	}
	for _, snap := range []domain.Snapshot{
		{ID: h('a'), DocHash: h('a'), Branch: "main", MemoryHash: baseHash},
		{ID: h('b'), DocHash: h('b'), Branch: "main", Parents: []domain.ContentHash{h('a')}, MemoryHash: removedHash},
		boundary,
	} {
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	stale.GraftCoverage = memoryGraftCoverage(ctx, st, boundary, stale.Fragments, true)
	if stale.GraftCoverage == nil {
		t.Fatal("complete stale boundary did not produce coverage")
	}
	staleHash, err := st.PutMemory(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	// Current seq=2 removes the old graft parent. Only A + D remain reachable.
	boundary.GraftSeq = 2
	boundary.GraftParents = nil
	boundary.Grafted = false
	boundary.MemoryHash = staleHash
	if err := st.PutSnapshot(ctx, boundary); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: h('e'), DocHash: h('e'), Branch: "main", Parents: []domain.ContentHash{h('d')}}); err != nil {
		t.Fatal(err)
	}
	child, _ := st.GetSnapshot(ctx, h('e'))
	got, ok := ancestorMemoryProjection(ctx, st, child)
	if !ok || !strings.Contains(got.Summary, "current base") || !strings.Contains(got.Summary, "boundary own work") {
		t.Fatalf("superseded projection = %q", got.Summary)
	}
	if strings.Contains(got.Summary, "removed graft work") {
		t.Fatalf("superseded graft survived projection: %q", got.Summary)
	}
}

func TestMemoryProjectionRetainsReachableFragmentWhenSourceMemoryIsUnavailable(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	base := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: h('a'), Summary: "reachable fallback memory"})
	baseHash, err := st.PutMemory(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	rootDigest := domain.MergeDigests(base, domain.MemoryDigest{SnapshotID: h('d'), Summary: "root memory"})
	baseSnapshot := domain.Snapshot{ID: h('a'), DocHash: h('a'), Branch: "main", MemoryHash: baseHash}
	root := domain.Snapshot{ID: h('d'), DocHash: h('d'), Branch: "main", Parents: []domain.ContentHash{h('a')}}
	for _, snap := range []domain.Snapshot{baseSnapshot, root} {
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	rootDigest.GraftCoverage = memoryGraftCoverage(ctx, st, root, rootDigest.Fragments, true)
	if rootDigest.GraftCoverage == nil || len(rootDigest.GraftCoverage.PinnedSources) != 0 {
		t.Fatalf("initial coverage = %+v, reachable readable base must not be pinned", rootDigest.GraftCoverage)
	}
	rootHash, err := st.PutMemory(ctx, rootDigest)
	if err != nil {
		t.Fatal(err)
	}
	root.MemoryHash = rootHash
	if err := st.PutSnapshot(ctx, root); err != nil {
		t.Fatal(err)
	}

	// Simulate a partial/corrupt pull after coverage was recorded. The source is
	// still a natural parent, but its current derivative memory blob is absent.
	// That missing frontier invalidates root coverage; stale repair must use the
	// fragment already embedded in rootDigest instead of silently losing it.
	root, err = st.GetSnapshot(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacement := domain.MemoryDigest{
		SnapshotID: h('a'), PreviousMemoryHash: baseHash, Summary: "new but unavailable parent memory",
	}
	replacementHash, err := st.PutMemory(ctx, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSwapSnapshotMemory(ctx, h('a'), baseHash, replacementHash); err != nil {
		t.Fatal(err)
	}
	partial := &missingSelectedMemoryStore{FileStore: st, missing: replacementHash}
	got, ok, complete := snapshotMemoryProjectionDetailed(ctx, partial, root)
	if !ok || complete || !strings.Contains(got.Summary, "reachable fallback memory") || !strings.Contains(got.Summary, "root memory") {
		t.Fatalf("partial-memory projection = %q, want reachable fallback + root", got.Summary)
	}
}

func TestMemoryProjectionRetainsDeepFragmentsWhenLineageIsPartial(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	putMemory := func(d domain.MemoryDigest) domain.ContentHash {
		hash, err := st.PutMemory(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	base := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: h('a'), Summary: "deep base memory"})
	middle := domain.MergeDigests(base, domain.MemoryDigest{SnapshotID: h('b'), Summary: "missing middle memory"})
	rootDigest := domain.MergeDigests(middle, domain.MemoryDigest{SnapshotID: h('d'), Summary: "partial root memory"})
	baseHash := putMemory(base)
	middleHash := putMemory(middle)
	root := domain.Snapshot{ID: h('d'), DocHash: h('d'), Branch: "main", Parents: []domain.ContentHash{h('b')}}
	for _, snap := range []domain.Snapshot{
		{ID: h('a'), DocHash: h('a'), Branch: "main", MemoryHash: baseHash},
		{ID: h('b'), DocHash: h('b'), Branch: "main", Parents: []domain.ContentHash{h('a')}, MemoryHash: middleHash},
		root,
	} {
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	rootDigest.GraftCoverage = memoryGraftCoverage(ctx, st, root, rootDigest.Fragments, true)
	if rootDigest.GraftCoverage == nil || len(rootDigest.GraftCoverage.PinnedSources) != 0 {
		t.Fatalf("initial complete coverage = %+v", rootDigest.GraftCoverage)
	}
	root.MemoryHash = putMemory(rootDigest)
	if err := st.PutSnapshot(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSnapshot(ctx, h('b')); err != nil {
		t.Fatal(err)
	}
	root, err := st.GetSnapshot(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, complete := snapshotMemoryProjectionDetailed(ctx, st, root)
	if !ok || complete {
		t.Fatal("partial lineage discarded the root's available digest")
	}
	for _, want := range []string{"deep base memory", "missing middle memory", "partial root memory"} {
		if count := strings.Count(got.Summary, want); count != 1 {
			t.Fatalf("partial projection contains %q %d times, want once: %q", want, count, got.Summary)
		}
	}
}

func TestMemoryProjectionInvalidatesWhenAncestorGraftChanges(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	putMemory := func(d domain.MemoryDigest) domain.ContentHash {
		hash, err := st.PutMemory(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	base := domain.MemoryDigest{SnapshotID: h('a'), Summary: "ancestor base"}
	teammate := domain.MergeDigests(base, domain.MemoryDigest{SnapshotID: h('c'), Summary: "late ancestor graft"})
	local := domain.MergeDigests(base, domain.MemoryDigest{SnapshotID: h('b'), Summary: "local ancestor"})
	childDigest := domain.MergeDigests(local, domain.MemoryDigest{SnapshotID: h('d'), Summary: "covered child"})
	baseHash := putMemory(base)
	teammateHash := putMemory(teammate)
	localHash := putMemory(local)
	for _, snap := range []domain.Snapshot{
		{ID: h('a'), DocHash: h('a'), Branch: "main", MemoryHash: baseHash},
		{ID: h('c'), DocHash: h('c'), Branch: "main", Parents: []domain.ContentHash{h('a')}, MemoryHash: teammateHash},
		{ID: h('b'), DocHash: h('b'), Branch: "main", Parents: []domain.ContentHash{h('a')}, MemoryHash: localHash},
		{ID: h('d'), DocHash: h('d'), Branch: "main", Parents: []domain.ContentHash{h('b')}},
	} {
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	child, _ := st.GetSnapshot(ctx, h('d'))
	childDigest.GraftCoverage = memoryGraftCoverage(ctx, st, child, childDigest.Fragments, true)
	if childDigest.GraftCoverage == nil {
		t.Fatal("initial child lineage did not produce coverage")
	}
	childHash := putMemory(childDigest)
	child.MemoryHash = childHash
	if err := st.PutSnapshot(ctx, child); err != nil {
		t.Fatal(err)
	}

	// The child itself is unchanged. Mutating B's graft register must still
	// invalidate D through the transitive lineage fingerprint.
	if err := st.PutSnapshot(ctx, domain.Snapshot{
		ID: h('b'), DocHash: h('b'), Branch: "main", Parents: []domain.ContentHash{h('a')},
		Grafted: true, GraftSeq: 1, GraftParents: []domain.ContentHash{h('c')},
	}); err != nil {
		t.Fatal(err)
	}
	child, _ = st.GetSnapshot(ctx, h('d'))
	got, ok := snapshotMemoryProjection(ctx, st, child)
	if !ok || !strings.Contains(got.Summary, "local ancestor") ||
		!strings.Contains(got.Summary, "late ancestor graft") || !strings.Contains(got.Summary, "covered child") {
		t.Fatalf("ancestor mutation projection = %q", got.Summary)
	}
}

func TestMemoryProjectionCoverageExcludesRootPointerButRejectsPartialLineage(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	h := func(c byte) domain.ContentHash {
		return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
	}
	root := domain.Snapshot{ID: h('a'), DocHash: h('a'), Branch: "main"}
	if err := st.PutSnapshot(ctx, root); err != nil {
		t.Fatal(err)
	}
	digest := domain.MemoryDigest{SnapshotID: root.ID, Summary: "root memory"}
	digest.GraftCoverage = memoryGraftCoverage(ctx, st, root, digest.Fragments, true)
	if digest.GraftCoverage == nil {
		t.Fatal("complete root did not produce coverage")
	}
	memoryHash, err := st.PutMemory(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	root.MemoryHash = memoryHash
	if err := st.PutSnapshot(ctx, root); err != nil {
		t.Fatal(err)
	}
	walker := memoryProjectionWalker{fingerprinter: newMemoryProjectionFingerprinter(ctx, st)}
	if !walker.memoryDigestCoversLineage(digest, root) {
		t.Fatal("attaching the digest's own MemoryHash invalidated its coverage")
	}
	incomplete := *digest.GraftCoverage
	incomplete.ProjectionComplete = false
	digest.GraftCoverage = &incomplete
	if walker.memoryDigestCoversLineage(digest, root) {
		t.Fatal("incomplete projection was trusted as a traversal stop")
	}
	future := *digest.GraftCoverage
	future.ProjectionComplete = true
	future.ProjectionVersion++
	digest.GraftCoverage = &future
	if walker.memoryDigestCoversLineage(digest, root) {
		t.Fatal("unknown future projection version was trusted as a stop point")
	}

	partial := domain.Snapshot{ID: h('b'), DocHash: h('b'), Parents: []domain.ContentHash{h('c')}}
	if coverage := memoryGraftCoverage(ctx, st, partial, nil, true); coverage == nil || coverage.ProjectionComplete || coverage.LineageFingerprint != "" {
		t.Fatalf("partial lineage produced trusted coverage: %+v", coverage)
	}
}

func TestMemorizeRecordsExactGraftCoverage(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := domain.Repo{ID: "repo-memory-coverage", DefaultBranch: "main", LocalPath: t.TempDir()}
	docHash, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex},
		Events:   []domain.Event{{Kind: domain.EventMessage, Role: "user", Seq: 0}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	graftParent := domain.HashContent([]byte("coverage-parent"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: graftParent, DocHash: graftParent, RepoID: repo.ID, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{
		ID: docHash, DocHash: docHash, RepoID: repo.ID, Branch: "main",
		Grafted: true, GraftSeq: 7, GraftParents: []domain.ContentHash{graftParent},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo.ID, Target: docHash}); err != nil {
		t.Fatal(err)
	}
	service := NewMemorizeService(
		branchSeedGit{repo: repo}, nil, nil, nil,
		stubDistiller{d: domain.MemoryDigest{Summary: "fresh covered memory"}}, st,
	)
	out, err := service.Memorize(ctx, inbound.MemorizeInput{Cwd: repo.LocalPath, Provider: domain.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := st.GetMemory(ctx, out.MemoryHash)
	if err != nil {
		t.Fatal(err)
	}
	if digest.GraftCoverage == nil || digest.GraftCoverage.ProjectionVersion != domain.MemoryProjectionVersion || !digest.GraftCoverage.ProjectionComplete || digest.GraftCoverage.LineageFingerprint == "" || digest.GraftCoverage.GraftSeq != 7 ||
		len(digest.GraftCoverage.GraftParents) != 1 || digest.GraftCoverage.GraftParents[0] != graftParent {
		t.Fatalf("memorize graft coverage = %+v", digest.GraftCoverage)
	}
	if len(digest.Fragments) != 1 || digest.Fragments[0].SourceSnapshot != docHash {
		t.Fatalf("memorize provenance = %+v, want current snapshot fragment", digest.Fragments)
	}
	second, err := service.Memorize(ctx, inbound.MemorizeInput{Cwd: repo.LocalPath, Provider: domain.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	if second.MemoryHash != out.MemoryHash {
		t.Fatalf("unchanged memorize created a new causal node: first=%s second=%s", out.MemoryHash, second.MemoryHash)
	}
}

func TestLegacyDigestContainmentIsExactAndPreservesStructure(t *testing.T) {
	projection := domain.MemoryDigest{
		Summary: "exact prior summary", KeyFacts: []string{"kept fact"}, OpenTasks: []string{"kept task"},
	}
	legacy := domain.MemoryDigest{
		Summary: "prefix\nexact prior summary\nnew tail", KeyFacts: []string{"kept fact"}, OpenTasks: []string{"kept task"},
	}
	if !legacyDigestContainsProjectionNarrative(legacy, projection) {
		t.Fatal("exact cumulative legacy digest did not absorb its prior projection")
	}
	missingFact := legacy
	missingFact.KeyFacts = nil
	walker := memoryProjectionWalker{projection: projection, found: true}
	walker.mergeLegacyOpaque(missingFact)
	if len(walker.projection.KeyFacts) != 1 || walker.projection.KeyFacts[0] != "kept fact" ||
		len(walker.projection.OpenTasks) != 1 || walker.projection.OpenTasks[0] != "kept task" {
		t.Fatalf("narrative containment discarded structure: %+v", walker.projection)
	}
	caseChanged := legacy
	caseChanged.Summary = "EXACT PRIOR SUMMARY"
	if legacyDigestContainsProjectionNarrative(caseChanged, projection) {
		t.Fatal("fuzzy/case-insensitive containment was accepted")
	}
}

// TestBoundCarriedDigestKeepsNewestTail (#33 — bounded carry): the forward
// working set is capped at memoryCarryBudgetBytes keeping the newest tail;
// under-budget digests pass through unchanged.
func TestBoundCarriedDigestKeepsNewestTail(t *testing.T) {
	small := domain.MemoryDigest{Summary: "compact history", KeyFacts: []string{"f"}}
	if got := boundCarriedDigest(small); got.Summary != small.Summary {
		t.Fatalf("under-budget summary changed: %q", got.Summary)
	}

	big := domain.MemoryDigest{
		Summary:   "OLDEST-GENERATION-HEAD\n" + strings.Repeat("m", 2*memoryCarryBudgetBytes) + "\nNEWEST-GENERATION-TAIL",
		KeyFacts:  []string{"fact stays"},
		OpenTasks: []string{"task stays"},
	}
	got := boundCarriedDigest(big)
	if len(got.Summary) > memoryCarryBudgetBytes {
		t.Fatalf("carried summary = %d bytes, want <= %d", len(got.Summary), memoryCarryBudgetBytes)
	}
	if !strings.Contains(got.Summary, "NEWEST-GENERATION-TAIL") {
		t.Fatal("bounded carry lost the newest generation")
	}
	if strings.Contains(got.Summary, "OLDEST-GENERATION-HEAD") {
		t.Fatal("bounded carry kept the oldest head instead of the newest tail")
	}
	if !strings.Contains(got.Summary, "[... earlier summary omitted ...]") {
		t.Fatal("bounded carry missing the omission marker")
	}
	if len(got.KeyFacts) != 1 || len(got.OpenTasks) != 1 {
		t.Fatalf("bullets changed: facts=%v tasks=%v", got.KeyFacts, got.OpenTasks)
	}
}

func TestBoundCarriedDigestCapsFragmentProjection(t *testing.T) {
	var fragments []domain.MemoryFragment
	for i := 0; i < 8; i++ {
		fragments = append(fragments, domain.MemoryFragment{
			SourceSnapshot: domain.ContentHash("sha256:" + strings.Repeat(string(rune('a'+i)), 64)),
			Summary:        strings.Repeat(string(rune('A'+i)), 80<<10),
		})
	}
	got := boundCarriedDigest(domain.MemoryDigest{Fragments: fragments})
	total := 0
	for _, fragment := range got.Fragments {
		total += len(fragment.Summary)
	}
	if total > memoryCarryBudgetBytes {
		t.Fatalf("fragment projection = %d bytes, want <= %d", total, memoryCarryBudgetBytes)
	}
	if len(got.Fragments) == 0 || !strings.Contains(got.Fragments[len(got.Fragments)-1].Summary, "H") {
		t.Fatal("fragment projection did not retain newest contribution")
	}
}

func TestBoundCarriedDigestDropsNestedSeedNarrativeButKeepsStructure(t *testing.T) {
	source := domain.ContentHash("sha256:" + strings.Repeat("a", 64))
	prior := domain.MemoryDigest{
		SnapshotID: source,
		Summary:    "project history\n" + seedSummaryPrefix + " 99 events omitted\nrecursive history",
		KeyFacts:   []string{"The immutable parent rule remains in force."},
		OpenTasks:  []string{"Verify the clean projection."},
		Fragments: []domain.MemoryFragment{{
			SourceSnapshot: source,
			Summary:        "project history\n" + seedSummaryPrefix + " 99 events omitted\nrecursive history",
			KeyFacts:       []string{"The immutable parent rule remains in force."},
			OpenTasks:      []string{"Verify the clean projection."},
		}},
	}
	carried := boundCarriedDigest(prior)
	fresh := domain.MemoryDigest{SnapshotID: domain.ContentHash("sha256:" + strings.Repeat("b", 64)), Summary: "clean current work"}
	got := domain.MergeDigests(carried, fresh)
	if strings.Contains(got.Summary, seedSummaryPrefix) || strings.Contains(got.Summary, "recursive history") {
		t.Fatalf("nested legacy narrative survived:\n%s", got.Summary)
	}
	if !strings.Contains(got.Summary, "clean current work") || len(got.KeyFacts) != 1 || len(got.OpenTasks) != 1 {
		t.Fatalf("clean projection lost structure: %+v", got)
	}
}

// A fresh generation is not a carried ancestor and must remain byte-for-byte
// intact even when it exceeds the carry budget. Only the inherited side is
// bounded before MergeDigests.
func TestBoundedCarryDoesNotTruncateFreshDigest(t *testing.T) {
	prior := domain.MemoryDigest{Summary: strings.Repeat("old", memoryCarryBudgetBytes)}
	freshText := "FRESH-HEAD\n" + strings.Repeat("new", memoryCarryBudgetBytes) + "\nFRESH-TAIL"
	fresh := domain.MemoryDigest{Summary: freshText}
	merged := domain.MergeDigests(boundCarriedDigest(prior), fresh)
	if !strings.Contains(merged.Summary, freshText) {
		t.Fatal("fresh digest was truncated while limiting inherited carry")
	}
}

func TestBoundCarriedDigestBoundsStructuredListsFromNewestTail(t *testing.T) {
	d := domain.MemoryDigest{
		KeyFacts:  []string{"old-fact", strings.Repeat("f", memoryCarryListBudgetBytes), "new-fact"},
		OpenTasks: []string{"old-task", strings.Repeat("t", memoryCarryListBudgetBytes), "new-task"},
	}
	got := boundCarriedDigest(d)
	if len(got.KeyFacts) != 1 || got.KeyFacts[0] != "new-fact" {
		t.Fatalf("facts carry = %v, want newest tail", got.KeyFacts)
	}
	if len(got.OpenTasks) != 1 || got.OpenTasks[0] != "new-task" {
		t.Fatalf("tasks carry = %v, want newest tail", got.OpenTasks)
	}
}
