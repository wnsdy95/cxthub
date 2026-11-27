package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type verifiedCommitBlobs struct {
	outbound.BlobStore
	writes int
}

func (s *verifiedCommitBlobs) PutDoc(context.Context, domain.ContentHash, domain.SessionDoc) (bool, error) {
	return false, errors.New("unexpected second validation through legacy port")
}
func (s *verifiedCommitBlobs) PutVerifiedDoc(ctx context.Context, repo domain.ContentHash, doc domain.VerifiedSessionDoc) (bool, error) {
	s.writes++
	return s.BlobStore.(outbound.VerifiedDocStore).PutVerifiedDoc(ctx, repo, doc)
}

func TestCommitUsesVerifiedDocumentAndRejectsForgedDuplicate(t *testing.T) {
	svc, st := newFsckSvc(t)
	repo := hh("verified-commit")
	bindCommitTestRepo(t, st, repo)
	writer := &verifiedCommitBlobs{BlobStore: st}
	svc.blobs = writer
	doc := makeCommitDoc(t, "original")
	input := inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{doc}}
	if _, err := svc.Commit(systemTestContext(), input); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 1 {
		t.Fatalf("validated writes = %d", writer.writes)
	}
	forged := makeCommitDoc(t, "different")
	forged.Hash = doc.Hash
	input.Docs = append(input.Docs, forged)
	if _, err := svc.Commit(systemTestContext(), input); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("forged duplicate: %v", err)
	}
	if writer.writes != 1 {
		t.Fatal("a document was written before validating the full batch")
	}
}

func makeCommitDoc(t *testing.T, text string) domain.SessionDoc {
	t.Helper()
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull, GitBranch: "main"},
		Events:   []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}},
	}
	cb, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	return domain.SessionDoc{Hash: domain.HashContent(cb), CIR: cir}
}

// A capture is no longer stored: the batch is refused whole, and the document
// that came with it is not written.
func TestCommitRefusesACapture(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh("capture-refused")
	bindCommitTestRepo(t, st, repo)
	doc := makeCommitDoc(t, "sent with a capture")
	snap := domain.Snapshot{ID: doc.Hash, RepoID: repo, Branch: "main", DocHash: doc.Hash, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}
	out, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{doc}, Snapshots: []domain.Snapshot{snap}})
	if !errors.Is(err, domain.ErrValidation) || out.StoredDocs != 0 {
		t.Fatalf("a capture: %+v %v", out, err)
	}
	if _, err := st.GetDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the document of a refused batch was stored: %v", err)
	}
}

func TestCommitRejectsDocHashMismatchBeforeStore(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh("commit-integrity-repo")
	doc := makeCommitDoc(t, "actual body")
	doc.Hash = hh("claimed-doc-hash")

	_, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{doc}})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity, got %v", err)
	}
	if _, gerr := st.GetDoc(ctx, repo, doc.Hash); !errors.Is(gerr, domain.ErrNotFound) {
		t.Fatalf("mismatched doc was stored: %v", gerr)
	}
	if _, gerr := st.GetRepo(ctx, repo); !errors.Is(gerr, domain.ErrNotFound) {
		t.Fatalf("failed commit created repo record: %v", gerr)
	}
}

func bindCommitTestRepo(t *testing.T, st interface {
	PutRepo(context.Context, domain.Repo) (domain.Repo, error)
}, repo domain.ContentHash) {
	t.Helper()
	if _, err := st.PutRepo(systemTestContext(), domain.Repo{
		ID: repo, DefaultBranch: "main", RepositoryID: domain.NewID("ws_"),
	}); err != nil {
		t.Fatal(err)
	}
}
