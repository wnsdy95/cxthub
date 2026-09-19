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

type docReadCountingStore struct {
	outbound.SessionStore
	reads      []domain.ContentHash
	beforeRead func(domain.ContentHash) error
}

func (s *docReadCountingStore) GetDoc(ctx context.Context, hash domain.ContentHash) (domain.SessionDoc, error) {
	if s.beforeRead != nil {
		if err := s.beforeRead(hash); err != nil {
			return domain.SessionDoc{}, err
		}
	}
	s.reads = append(s.reads, hash)
	return s.SessionStore.GetDoc(ctx, hash)
}

type lazyPushRemote struct {
	outbound.RemoteSync
	wants           outbound.PushObjectWants
	snapshotHaves   []domain.ContentHash
	docHaves        []domain.ContentHash
	objectCalls     int
	refCalls        int
	objectSnapshots []domain.Snapshot
	objectDocs      []domain.SessionDoc
	onPush          func([]domain.SessionDoc) error
}

func (r *lazyPushRemote) NegotiatePushObjects(_ context.Context, _ string, snapshotHaves, docHaves []domain.ContentHash) (outbound.PushObjectWants, error) {
	r.snapshotHaves = append([]domain.ContentHash(nil), snapshotHaves...)
	r.docHaves = append([]domain.ContentHash(nil), docHaves...)
	return r.wants, nil
}

func (r *lazyPushRemote) Push(_ context.Context, _ string, snapshots []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, _, _ bool) error {
	if r.onPush != nil {
		if err := r.onPush(docs); err != nil {
			return err
		}
	}
	if len(snapshots) > 0 || len(docs) > 0 {
		r.objectCalls++
		r.objectSnapshots = append(r.objectSnapshots, snapshots...)
		r.objectDocs = append(r.objectDocs, docs...)
	}
	if len(refs) > 0 {
		r.refCalls++
	}
	return nil
}

func TestPushUploadsEachDocumentBeforeReadingTheNext(t *testing.T) {
	base, repoID, ids := lazyPushFixture(t)
	remote := &lazyPushRemote{wants: outbound.PushObjectWants{Snapshots: ids, Docs: ids}}
	counting := &docReadCountingStore{SessionStore: base}
	counting.beforeRead = func(domain.ContentHash) error {
		if len(counting.reads) != len(remote.objectDocs) {
			return errors.New("previous document still buffered")
		}
		return nil
	}
	_, err := newTestSyncService(counting, remote, nil).Push(context.Background(), inbound.SyncInput{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if len(counting.reads) != 2 || remote.refCalls != 1 {
		t.Fatalf("reads=%v refs=%d", counting.reads, remote.refCalls)
	}
}

func TestPushStopsBeforeNextDocumentAndRefsOnFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport", true: "cancellation"}[canceled], func(t *testing.T) {
			base, repoID, ids := lazyPushFixture(t)
			counting := &docReadCountingStore{SessionStore: base}
			remote := &lazyPushRemote{wants: outbound.PushObjectWants{Snapshots: ids, Docs: ids}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("upload failed")
			remote.onPush = func(docs []domain.SessionDoc) error {
				if len(docs) == 0 {
					return nil
				}
				if canceled {
					cancel()
					return nil
				}
				return failure
			}
			_, err := newTestSyncService(counting, remote, nil).Push(ctx, inbound.SyncInput{RepoID: repoID})
			want := failure
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			if len(counting.reads) != 1 || remote.refCalls != 0 || len(remote.objectSnapshots) != 0 {
				t.Fatalf("after failure reads=%v refs=%d snapshots=%d", counting.reads, remote.refCalls, len(remote.objectSnapshots))
			}
		})
	}
}

func TestPushResumesCompletedDocumentsWithoutAdvancingRefsEarly(t *testing.T) {
	base, repoID, ids := lazyPushFixture(t)
	counting := &docReadCountingStore{SessionStore: base}
	remote := &lazyPushRemote{wants: outbound.PushObjectWants{Snapshots: ids, Docs: ids}}
	remote.onPush = func(docs []domain.SessionDoc) error {
		if len(docs) > 0 && len(remote.objectDocs) == 1 {
			return errors.New("second document unavailable")
		}
		return nil
	}
	svc := newTestSyncService(counting, remote, nil)
	if _, err := svc.Push(context.Background(), inbound.SyncInput{RepoID: repoID}); err == nil {
		t.Fatal("failed upload succeeded")
	}
	if len(remote.objectDocs) != 1 || len(remote.objectSnapshots) != 0 || remote.refCalls != 0 {
		t.Fatal("incomplete objects advanced snapshot/ref phase")
	}
	remaining := counting.reads[1]
	counting.reads = nil
	remote.wants.Docs = []domain.ContentHash{remaining}
	remote.onPush = nil
	if _, err := svc.Push(context.Background(), inbound.SyncInput{RepoID: repoID}); err != nil {
		t.Fatal(err)
	}
	if len(counting.reads) != 1 || counting.reads[0] != remaining || len(remote.objectDocs) != 2 || len(remote.objectSnapshots) != 2 || remote.refCalls != 1 {
		t.Fatalf("retry reads=%v docs=%d snapshots=%d refs=%d", counting.reads, len(remote.objectDocs), len(remote.objectSnapshots), remote.refCalls)
	}
}

func (r *lazyPushRemote) DeleteUnsyncRemote(context.Context, string, string) error { return nil }

func lazyPushFixture(t *testing.T) (*storage.FileStore, string, []domain.ContentHash) {
	t.Helper()
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("lazy-push-repo")))
	var ids []domain.ContentHash
	var parents []domain.ContentHash
	for _, marker := range []string{"first", "second"} {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{
			Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: marker},
			Events: []domain.Event{{
				Kind: domain.EventMessage, Seq: 0, Role: "user",
				Blocks: []domain.ContentBlock{{Type: "text", Text: marker}},
			}},
		}}
		id, err := store.PutDoc(ctx, doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutSnapshot(ctx, domain.Snapshot{
			ID: id, RepoID: repoID, Branch: "main", Parents: parents, DocHash: id,
		}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		parents = []domain.ContentHash{id}
	}
	if err := store.PutRef(ctx, domain.Ref{
		Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: ids[len(ids)-1],
	}); err != nil {
		t.Fatal(err)
	}
	return store, repoID, ids
}

func TestPushLoadsOnlyServerRequestedDocuments(t *testing.T) {
	tests := []struct {
		name          string
		wants         func([]domain.ContentHash) outbound.PushObjectWants
		wantDocReads  int
		wantSnapshots int
		wantDocs      int
	}{
		{
			name:  "no-op",
			wants: func([]domain.ContentHash) outbound.PushObjectWants { return outbound.PushObjectWants{} },
		},
		{
			name: "doc-only repair",
			wants: func(ids []domain.ContentHash) outbound.PushObjectWants {
				return outbound.PushObjectWants{Docs: []domain.ContentHash{ids[0]}}
			},
			wantDocReads: 1, wantDocs: 1,
		},
		{
			name: "snapshot-only repair",
			wants: func(ids []domain.ContentHash) outbound.PushObjectWants {
				return outbound.PushObjectWants{Snapshots: []domain.ContentHash{ids[1]}}
			},
			wantSnapshots: 1,
		},
		{
			name: "new repository",
			wants: func(ids []domain.ContentHash) outbound.PushObjectWants {
				return outbound.PushObjectWants{Snapshots: ids, Docs: ids}
			},
			wantDocReads: 2, wantSnapshots: 2, wantDocs: 2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base, repoID, ids := lazyPushFixture(t)
			counting := &docReadCountingStore{SessionStore: base}
			remote := &lazyPushRemote{wants: test.wants(ids)}
			out, err := newTestSyncService(counting, remote, nil).Push(context.Background(), inbound.SyncInput{RepoID: repoID})
			if err != nil {
				t.Fatal(err)
			}
			if len(counting.reads) != test.wantDocReads {
				t.Fatalf("GetDoc calls=%v, want %d", counting.reads, test.wantDocReads)
			}
			if len(remote.snapshotHaves) != 2 || len(remote.docHaves) != 2 {
				t.Fatalf("inventory snapshots=%v docs=%v", remote.snapshotHaves, remote.docHaves)
			}
			if len(remote.objectSnapshots) != test.wantSnapshots || len(remote.objectDocs) != test.wantDocs {
				t.Fatalf("objects snapshots=%d docs=%d, want %d/%d", len(remote.objectSnapshots), len(remote.objectDocs), test.wantSnapshots, test.wantDocs)
			}
			wantObjectCalls := test.wantDocs
			if test.wantSnapshots > 0 {
				wantObjectCalls++
			}
			if remote.objectCalls != wantObjectCalls || remote.refCalls != 1 {
				t.Fatalf("calls objects=%d refs=%d, want %d/1", remote.objectCalls, remote.refCalls, wantObjectCalls)
			}
			if out.Pushed != test.wantSnapshots {
				t.Fatalf("reported pushed=%d, want %d", out.Pushed, test.wantSnapshots)
			}
		})
	}
}

func TestPushRejectsWantOutsideAdvertisedInventoryBeforeDocRead(t *testing.T) {
	base, repoID, _ := lazyPushFixture(t)
	counting := &docReadCountingStore{SessionStore: base}
	remote := &lazyPushRemote{wants: outbound.PushObjectWants{
		Docs: []domain.ContentHash{domain.HashContent([]byte("not advertised"))},
	}}
	_, err := newTestSyncService(counting, remote, nil).Push(context.Background(), inbound.SyncInput{RepoID: repoID})
	if !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("push error=%v, want hash mismatch", err)
	}
	if len(counting.reads) != 0 || remote.objectCalls != 0 || remote.refCalls != 0 {
		t.Fatalf("untrusted wants caused reads/publication: reads=%v objects=%d refs=%d", counting.reads, remote.objectCalls, remote.refCalls)
	}
}
