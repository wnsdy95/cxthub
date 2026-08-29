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

func pullDoc(t *testing.T, text string) domain.SessionDoc {
	t.Helper()
	cir := domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull},
		Events:   []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}},
	}
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	return domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: cir}
}

func TestValidatePullBatchRejectsTamperingAndBrokenGraph(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("pull-repo")))
	doc := pullDoc(t, "valid")
	root := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main"}
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: root.ID}

	t.Run("valid", func(t *testing.T) {
		st := storage.NewFileStore(t.TempDir())
		if err := validatePullBatch(ctx, st, repo, []domain.Snapshot{root}, []domain.SessionDoc{doc}, []domain.Ref{ref}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("tampered doc", func(t *testing.T) {
		st := storage.NewFileStore(t.TempDir())
		bad := pullDoc(t, "changed")
		bad.Hash = doc.Hash
		if err := validatePullBatch(ctx, st, repo, []domain.Snapshot{root}, []domain.SessionDoc{bad}, []domain.Ref{ref}); !errors.Is(err, domain.ErrHashMismatch) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("missing parent", func(t *testing.T) {
		st := storage.NewFileStore(t.TempDir())
		child := root
		child.Parents = []domain.ContentHash{domain.HashContent([]byte("missing"))}
		if err := validatePullBatch(ctx, st, repo, []domain.Snapshot{child}, []domain.SessionDoc{doc}, []domain.Ref{ref}); !errors.Is(err, domain.ErrHashMismatch) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("immutable parent disagreement", func(t *testing.T) {
		st := storage.NewFileStore(t.TempDir())
		if err := st.PutSnapshot(ctx, root); err != nil {
			t.Fatal(err)
		}
		changed := root
		changed.Parents = []domain.ContentHash{domain.HashContent([]byte("other-parent"))}
		if err := validatePullBatch(ctx, st, repo, []domain.Snapshot{changed}, []domain.SessionDoc{doc}, []domain.Ref{ref}); !errors.Is(err, domain.ErrHashMismatch) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("legacy server may retain a branch projection shadowed by archive", func(t *testing.T) {
		st := storage.NewFileStore(t.TempDir())
		archive, err := domain.NewBranchLifecycleRef(repo, ref.Name, ref.Target, 1, domain.BranchArchived)
		if err != nil {
			t.Fatal(err)
		}
		if err := validatePullBatch(ctx, st, repo, []domain.Snapshot{root}, []domain.SessionDoc{doc}, []domain.Ref{archive, ref}); err != nil {
			t.Fatalf("legacy lifecycle projection rejected: %v", err)
		}
	})

	t.Run("same generation active lifecycle permits branch projection", func(t *testing.T) {
		st := storage.NewFileStore(t.TempDir())
		archive, err := domain.NewBranchLifecycleRef(repo, ref.Name, ref.Target, 1, domain.BranchArchived)
		if err != nil {
			t.Fatal(err)
		}
		active, err := domain.NewBranchLifecycleRef(repo, ref.Name, ref.Target, 1, domain.BranchActive)
		if err != nil {
			t.Fatal(err)
		}
		if err := validatePullBatch(ctx, st, repo, []domain.Snapshot{root}, []domain.SessionDoc{doc}, []domain.Ref{archive, active, ref}); err != nil {
			t.Fatalf("active winner rejected: %v", err)
		}
	})
}

type corruptMemoryRemote struct {
	outbound.RemoteSync
	snap   domain.Snapshot
	doc    domain.SessionDoc
	ref    domain.Ref
	memory domain.MemoryDigest
}

type inventoryPullRemote struct {
	outbound.RemoteSync
	states   map[domain.ContentHash]domain.ContentHash
	docHaves []domain.ContentHash
	refs     []domain.Ref
}

func (r *inventoryPullRemote) Pull(_ context.Context, _ string, states map[domain.ContentHash]domain.ContentHash, docHaves []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	r.states = states
	r.docHaves = append([]domain.ContentHash{}, docHaves...)
	return nil, nil, r.refs, nil
}

func TestSyncPullAdvertisesSnapshotStatesAndVerifiedDocs(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("pull-inventory-repo")))
	doc := pullDoc(t, "already synchronized")
	snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main", Message: "commit", GraftSeq: 2}
	st := storage.NewFileStore(t.TempDir())
	if _, err := st.PutDoc(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: snap.ID}
	if err := st.PutRef(ctx, ref); err != nil {
		t.Fatal(err)
	}
	remote := &inventoryPullRemote{refs: []domain.Ref{ref}}
	out, err := NewSyncRepoService(st, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repo, FetchOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	wantState, err := domain.SnapshotStateHash(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(remote.states) != 1 || remote.states[snap.ID] != wantState {
		t.Fatalf("advertised states = %+v, want %s=%s", remote.states, snap.ID, wantState)
	}
	if len(remote.docHaves) != 1 || remote.docHaves[0] != doc.Hash {
		t.Fatalf("advertised docs = %v", remote.docHaves)
	}
	if out.Pulled != 0 || len(out.RemoteAhead) != 0 {
		t.Fatalf("unchanged pull output = %+v", out)
	}
}

func (r *corruptMemoryRemote) Pull(context.Context, string, map[domain.ContentHash]domain.ContentHash, []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	return []domain.Snapshot{r.snap}, []domain.SessionDoc{r.doc}, []domain.Ref{r.ref}, nil
}

func (r *corruptMemoryRemote) PullMemory(context.Context, string, domain.ContentHash) (domain.MemoryDigest, error) {
	return r.memory, nil
}

func TestPullRejectsMemoryHashMismatchBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("memory-pull-repo")))
	doc := pullDoc(t, "memory")
	digest := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "real memory"}
	snap := domain.Snapshot{
		ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main",
		MemoryHash: domain.HashContent([]byte("claimed memory")),
	}
	remote := &corruptMemoryRemote{
		snap: snap, doc: doc, memory: digest,
		ref: domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: snap.ID},
	}
	st := storage.NewFileStore(t.TempDir())
	svc := NewSyncRepoService(st, remote, nil)
	_, err := svc.Pull(ctx, inbound.SyncInput{RepoID: repo})
	if !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("pull error = %v", err)
	}
	if _, err := st.GetDoc(ctx, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("doc was partially written: %v", err)
	}
	if _, err := st.GetSnapshot(ctx, snap.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("snapshot was partially written: %v", err)
	}
}

func TestPullDoesNotReplaceUntrackedLocalMemoryWithDifferentRemotePointer(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("memory-pull-conflict-repo")))
	doc := pullDoc(t, "same immutable session body")
	localMemory := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "new local memory"}
	remoteMemory := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "stale remote memory"}
	localHash, err := domain.MemoryDigestHash(localMemory)
	if err != nil {
		t.Fatal(err)
	}
	remoteHash, err := domain.MemoryDigestHash(remoteMemory)
	if err != nil {
		t.Fatal(err)
	}

	st := storage.NewFileStore(t.TempDir())
	if _, err := st.PutDoc(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if stored, err := st.PutMemory(ctx, localMemory); err != nil || stored != localHash {
		t.Fatalf("put local memory: hash=%s err=%v", stored, err)
	}
	localSnapshot := domain.Snapshot{
		ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main", MemoryHash: localHash,
	}
	if err := st.PutSnapshot(ctx, localSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: doc.Hash}); err != nil {
		t.Fatal(err)
	}

	remoteSnapshot := localSnapshot
	remoteSnapshot.MemoryHash = remoteHash
	remote := &corruptMemoryRemote{
		snap: remoteSnapshot, doc: doc, memory: remoteMemory,
		ref: domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: doc.Hash},
	}
	svc := NewSyncRepoService(st, remote, nil)
	if _, err := svc.Pull(ctx, inbound.SyncInput{RepoID: repo}); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("pull error = %v, want memory attachment conflict", err)
	}
	got, err := st.GetSnapshot(ctx, doc.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryHash != localHash {
		t.Fatalf("pull rolled local memory back: got %s want %s", got.MemoryHash, localHash)
	}
	if _, err := svc.Pull(ctx, inbound.SyncInput{RepoID: repo, Force: true}); err != nil {
		t.Fatalf("forced pull did not resolve memory fork: %v", err)
	}
	got, err = st.GetSnapshot(ctx, doc.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryHash != remoteHash {
		t.Fatalf("forced pull pointer=%s want remote %s", got.MemoryHash, remoteHash)
	}
	if _, err := st.GetMemory(ctx, localHash); err != nil {
		t.Fatalf("forced pull deleted losing immutable local memory: %v", err)
	}
}
