package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

type cancelAfterVerifiedDoc struct {
	*storage.FileStore
	cancel context.CancelFunc
}

func (s *cancelAfterVerifiedDoc) VerifyStoredDoc(ctx context.Context, id domain.ContentHash) error {
	if err := s.FileStore.VerifyStoredDoc(ctx, id); err != nil {
		return err
	}
	s.cancel()
	return ctx.Err()
}

type metadataOnlyProgressRemote struct {
	outbound.RemoteSync
	snaps []domain.Snapshot
	refs  []domain.Ref
}

func (r *metadataOnlyProgressRemote) Pull(context.Context, string, map[domain.ContentHash]domain.ContentHash, []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	return r.snaps, nil, r.refs, nil
}

func TestMetadataPullResumesVerificationWithoutPublishingPartialState(t *testing.T) {
	root := t.TempDir()
	keyPath := filepath.Join(t.TempDir(), "private", "key")
	st := storage.NewFileStore(root)
	st.EnableDocVerificationCache(keyPath)
	repo := string(domain.HashContent([]byte(t.Name())))
	remote := &metadataOnlyProgressRemote{}
	for _, message := range []string{"first historical transcript", "second historical transcript"} {
		doc := pullDoc(t, message)
		if _, err := st.PutDoc(context.Background(), doc); err != nil {
			t.Fatal(err)
		}
		snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main"}
		if len(remote.snaps) > 0 {
			snap.Parents = []domain.ContentHash{remote.snaps[0].ID}
		}
		remote.snaps = append(remote.snaps, snap)
	}
	remote.refs = []domain.Ref{{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: remote.snaps[1].ID}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := newTestSyncService(&cancelAfterVerifiedDoc{FileStore: st, cancel: cancel}, remote, nil)
	if _, err := svc.Pull(ctx, inbound.SyncInput{RepoID: repo}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	for _, snap := range remote.snaps {
		if _, err := st.GetSnapshot(context.Background(), snap.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("partial snapshot adopted", err)
		}
	}
	man, err := st.Manifest(context.Background(), repo)
	if err != nil || len(man.Refs) != 0 {
		t.Fatal("partial refs adopted", err)
	}
	cursors, err := st.LoadRemoteSnapshotStateCursor(context.Background(), repo)
	if err != nil || len(cursors) != 0 {
		t.Fatal("partial cursor adopted", err)
	}
	proof := filepath.Join(root, ".cxt", "doc-verification", strings.TrimPrefix(string(remote.snaps[0].ID), "sha256:")+".json")
	old := time.Unix(100, 0)
	if err := os.Chtimes(proof, old, old); err != nil {
		t.Fatal("completed validation was not checkpointed", err)
	}
	// A fresh store models the next CLI process, with no in-memory verified map.
	fresh := storage.NewFileStore(root)
	fresh.EnableDocVerificationCache(keyPath)
	if _, err := newTestSyncService(fresh, remote, nil).Pull(context.Background(), inbound.SyncInput{RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(proof)
	if err != nil || !info.ModTime().Equal(old) {
		t.Fatal("retry repeated completed canonical verification", err)
	}
	for _, snap := range remote.snaps {
		if _, err := fresh.GetSnapshot(context.Background(), snap.ID); err != nil {
			t.Fatal(err)
		}
	}
	man, err = fresh.Manifest(context.Background(), repo)
	if err != nil || len(man.Refs) == 0 {
		t.Fatal("complete pull failed to adopt refs", err)
	}
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
