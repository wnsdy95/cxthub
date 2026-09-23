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

type interruptedDocumentPull struct {
	outbound.RemoteSync
	doc      domain.SessionDoc
	fail     error
	reads    int
	snapshot domain.Snapshot
	ref      domain.Ref
}

func (r *interruptedDocumentPull) PullTo(ctx context.Context, _ string, _ map[domain.ContentHash]domain.ContentHash, _ []domain.ContentHash, receiver outbound.PullDocumentReceiver) ([]domain.Snapshot, []domain.Ref, error) {
	has, err := receiver.HasVerifiedDoc(ctx, r.doc.Hash)
	if err != nil {
		return nil, nil, err
	}
	if !has {
		r.reads++
		if err = receiver.ReceiveDoc(ctx, r.doc); err != nil {
			return nil, nil, err
		}
	}
	if r.fail != nil {
		return nil, nil, r.fail
	}
	return []domain.Snapshot{r.snapshot}, []domain.Ref{r.ref}, nil
}
func TestStreamingPullStagesBodiesBeforePublishingMetadata(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	doc := pullDoc(t, "staged transcript")
	remote := &interruptedDocumentPull{doc: doc, fail: context.DeadlineExceeded, snapshot: domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo}, ref: domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: doc.Hash}}
	svc := newTestSyncService(st, remote, nil)
	if _, err := svc.Pull(ctx, inbound.SyncInput{RepoID: repo, FetchOnly: true}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pull: %v", err)
	}
	if _, err := st.GetDoc(ctx, doc.Hash); err != nil {
		t.Fatal("validated body was not retained", err)
	}
	if _, err := st.GetSnapshot(ctx, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("partial metadata published: %v", err)
	}
	man, err := st.Manifest(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Refs) > 0 {
		t.Fatal("partial refs published")
	}
	remote.fail = nil
	if _, err = svc.Pull(ctx, inbound.SyncInput{RepoID: repo, FetchOnly: true}); err != nil {
		t.Fatal(err)
	}
	if remote.reads != 1 {
		t.Fatalf("body downloaded %d times", remote.reads)
	}
	if _, err = st.GetSnapshot(ctx, doc.Hash); err != nil {
		t.Fatal(err)
	}
}
func TestPullReceiverRejectsCorruptBodyBeforeStaging(t *testing.T) {
	st := storage.NewFileStore(t.TempDir())
	receiver := &pullDocumentReceiver{store: st, verified: map[domain.ContentHash]bool{}}
	doc := pullDoc(t, "original")
	doc.CIR.Events[0].Blocks[0].Text = "tampered"
	if err := receiver.ReceiveDoc(context.Background(), doc); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("tampered body: %v", err)
	}
	if _, err := st.GetDoc(context.Background(), doc.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("corrupt body stored: %v", err)
	}
}
