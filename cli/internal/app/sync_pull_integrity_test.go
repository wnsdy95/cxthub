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
		bad := doc
		bad.CIR.Events[0].Blocks[0].Text = "changed"
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
}

type corruptMemoryRemote struct {
	outbound.RemoteSync
	snap   domain.Snapshot
	doc    domain.SessionDoc
	ref    domain.Ref
	memory domain.MemoryDigest
}

func (r *corruptMemoryRemote) Pull(context.Context, string, []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
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
