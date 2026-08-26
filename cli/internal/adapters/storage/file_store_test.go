package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func sampleCIR(text string) domain.CIRDocument {
	return domain.CIRDocument{
		Envelope: domain.Envelope{
			CIRVersion:     "1",
			SourceProvider: domain.ProviderClaude,
			Fidelity:       domain.FidelityFull,
			GitBranch:      "main",
		},
		Events: []domain.Event{
			{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}},
		},
	}
}

func TestPutGetDocAndDedup(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())

	doc := domain.SessionDoc{CIR: sampleCIR("hello")}
	h1, err := store.PutDoc(ctx, doc)
	if err != nil {
		t.Fatalf("PutDoc: %v", err)
	}
	if h1 == "" {
		t.Fatal("empty hash")
	}
	// content-addressing: same content → same hash (idempotent dedup)
	h2, err := store.PutDoc(ctx, domain.SessionDoc{CIR: sampleCIR("hello")})
	if err != nil {
		t.Fatalf("PutDoc2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("dedup broken: %s != %s", h1, h2)
	}
	// objects/docs must contain only one file
	entries, _ := os.ReadDir(filepath.Join(store.storeDir(), "objects", "docs"))
	if len(entries) != 1 {
		t.Fatalf("expected 1 deduped object, got %d", len(entries))
	}
	// GetDoc round trip: rehash must be the same
	got, err := store.GetDoc(ctx, h1)
	if err != nil {
		t.Fatalf("GetDoc: %v", err)
	}
	cb, _ := domain.CanonicalBytes(got.CIR)
	if domain.HashContent(cb) != h1 {
		t.Fatalf("roundtrip hash mismatch")
	}
	// different content → different hash
	h3, _ := store.PutDoc(ctx, domain.SessionDoc{CIR: sampleCIR("world")})
	if h3 == h1 {
		t.Fatal("different content must yield different hash")
	}
}

func TestGetDocNotFound(t *testing.T) {
	store := NewFileStore(t.TempDir())
	_, err := store.GetDoc(context.Background(), domain.ContentHash("sha256:"+strings.Repeat("d", 64)))
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSnapshotAndRefs(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	h, _ := store.PutDoc(ctx, domain.SessionDoc{CIR: sampleCIR("x")})
	repoID := string(domain.HashContent([]byte("r1")))

	snap := domain.Snapshot{ID: h, RepoID: repoID, Branch: "main", DocHash: h, Provider: domain.ProviderClaude, Message: "first"}
	if err := store.PutSnapshot(ctx, snap); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	got, err := store.GetSnapshot(ctx, h)
	if err != nil || got.Branch != "main" || got.Message != "first" {
		t.Fatalf("GetSnapshot: %v %+v", err, got)
	}

	// branch ref
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: h}); err != nil {
		t.Fatalf("PutRef branch: %v", err)
	}
	ref, err := store.GetRef(ctx, repoID, domain.RefBranch, "main")
	if err != nil || ref.Target != h {
		t.Fatalf("GetRef branch: %v %+v", err, ref)
	}
	// nested branch name
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "feat/auth", RepoID: repoID, Target: h}); err != nil {
		t.Fatalf("PutRef nested: %v", err)
	}
	if r, err := store.GetRef(ctx, repoID, domain.RefBranch, "feat/auth"); err != nil || r.Target != h {
		t.Fatalf("GetRef nested: %v %+v", err, r)
	}
	// partial join remainder: actual git branch and session ref separated
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefSession, Name: "fork/abc", RepoID: repoID, Target: h}); err != nil {
		t.Fatalf("PutRef session: %v", err)
	}
	if r, err := store.GetRef(ctx, repoID, domain.RefSession, "fork/abc"); err != nil || r.Target != h {
		t.Fatalf("GetRef session: %v %+v", err, r)
	}
	// HEAD symbolic
	if err := store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repoID, Symbolic: "main"}); err != nil {
		t.Fatalf("PutRef HEAD: %v", err)
	}
	head, err := store.GetRef(ctx, repoID, domain.RefHEAD, "HEAD")
	if err != nil || head.Symbolic != "main" {
		t.Fatalf("GetRef HEAD: %v %+v", err, head)
	}

	// ListSnapshots branch filter
	if list, _ := store.ListSnapshots(ctx, repoID, "main"); len(list) != 1 {
		t.Fatalf("ListSnapshots main: expected 1, got %d", len(list))
	}
	if list, _ := store.ListSnapshots(ctx, repoID, "nope"); len(list) != 0 {
		t.Fatalf("ListSnapshots nope: expected 0, got %d", len(list))
	}

	// Manifest includes ref + snapshot index
	man, err := store.Manifest(ctx, repoID)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(man.SnapshotIndex) != 1 || len(man.Refs) < 2 {
		t.Fatalf("Manifest contents: idx=%d refs=%d", len(man.SnapshotIndex), len(man.Refs))
	}
}

func TestPutSnapshotEqualVersionUsesServerProjection(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	id := domain.HashContent([]byte("equal-version"))
	removed := domain.HashContent([]byte("superseded-parent"))
	repoID := string(domain.HashContent([]byte("repo")))
	local := domain.Snapshot{
		ID: id, RepoID: repoID, Branch: "main", DocHash: id,
		Grafted: true, GraftParents: []domain.ContentHash{removed}, GraftSeq: 4,
	}
	if err := store.PutSnapshot(ctx, local); err != nil {
		t.Fatal(err)
	}
	// Server join projecting edge removal at equal-seq
	// If union, removed revives, so entire incoming server set must be adopted
	authoritative := local
	authoritative.Grafted = false
	authoritative.GraftParents = nil
	if err := store.PutSnapshot(ctx, authoritative); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSnapshot(ctx, id)
	if err != nil || got.Grafted || len(got.GraftParents) != 0 || got.GraftSeq != 4 {
		t.Fatalf("equal-version server projection not adopted: %+v err=%v", got, err)
	}
}

func TestPutSnapshotLegacyZeroVersionStillMergesAdds(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	id := domain.HashContent([]byte("legacy-version"))
	p1 := domain.HashContent([]byte("legacy-parent-1"))
	p2 := domain.HashContent([]byte("legacy-parent-2"))
	base := domain.Snapshot{ID: id, DocHash: id, Branch: "main", Grafted: true, GraftParents: []domain.ContentHash{p1}}
	if err := store.PutSnapshot(ctx, base); err != nil {
		t.Fatal(err)
	}
	incoming := base
	incoming.GraftParents = []domain.ContentHash{p2}
	if err := store.PutSnapshot(ctx, incoming); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSnapshot(ctx, id)
	if err != nil || len(got.GraftParents) != 2 {
		t.Fatalf("legacy seq=0 additive merge lost: %+v err=%v", got, err)
	}
}

func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	d := domain.MemoryDigest{Summary: "did stuff", KeyFacts: []string{"a", "b"}, Provider: domain.ProviderClaude}
	h, err := store.PutMemory(ctx, d)
	if err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	got, err := store.GetMemory(ctx, h)
	if err != nil || got.Summary != "did stuff" || len(got.KeyFacts) != 2 {
		t.Fatalf("GetMemory: %v %+v", err, got)
	}
}

func TestCompareAndSwapSnapshotMemoryRejectsStaleWriter(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	firstStore := NewFileStore(root)
	secondStore := NewFileStore(root)
	id := domain.HashContent([]byte("memory-cas-snapshot"))
	if err := firstStore.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	first := domain.MemoryDigest{SnapshotID: id, Summary: "first contender"}
	second := domain.MemoryDigest{SnapshotID: id, Summary: "second contender"}
	firstHash, err := firstStore.PutMemory(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := secondStore.PutMemory(ctx, second)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, contender := range []struct {
		store *FileStore
		hash  domain.ContentHash
	}{{firstStore, firstHash}, {secondStore, secondHash}} {
		go func(store *FileStore, hash domain.ContentHash) {
			<-start
			errs <- store.CompareAndSwapSnapshotMemory(ctx, id, "", hash)
		}(contender.store, contender.hash)
	}
	close(start)
	var successes, conflicts int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			successes++
		case errors.Is(err, domain.ErrSyncConflict):
			conflicts++
		default:
			t.Fatalf("unexpected CAS error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CAS results: successes=%d conflicts=%d", successes, conflicts)
	}
	got, err := firstStore.GetSnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryHash != firstHash && got.MemoryHash != secondHash {
		t.Fatalf("unexpected final memory pointer %s", got.MemoryHash)
	}
}

func TestConcurrentSnapshotDedupRejectsDifferentInitialMemories(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	firstStore := NewFileStore(root)
	secondStore := NewFileStore(root)
	id := domain.HashContent([]byte("dedup-memory-snapshot"))
	firstHash, err := firstStore.PutMemory(ctx, domain.MemoryDigest{SnapshotID: id, Summary: "first initial"})
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := secondStore.PutMemory(ctx, domain.MemoryDigest{SnapshotID: id, Summary: "second initial"})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, contender := range []struct {
		store *FileStore
		hash  domain.ContentHash
	}{{firstStore, firstHash}, {secondStore, secondHash}} {
		go func(store *FileStore, hash domain.ContentHash) {
			<-start
			errs <- store.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, Branch: "main", MemoryHash: hash})
		}(contender.store, contender.hash)
	}
	close(start)
	var successes, conflicts int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			successes++
		case errors.Is(err, domain.ErrSyncConflict):
			conflicts++
		default:
			t.Fatalf("unexpected PutSnapshot error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("dedup results: successes=%d conflicts=%d", successes, conflicts)
	}
}
