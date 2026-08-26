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
	reads []domain.ContentHash
}

func (s *docReadCountingStore) GetDoc(ctx context.Context, hash domain.ContentHash) (domain.SessionDoc, error) {
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
}

func (r *lazyPushRemote) NegotiatePushObjects(_ context.Context, _ string, snapshotHaves, docHaves []domain.ContentHash) (outbound.PushObjectWants, error) {
	r.snapshotHaves = append([]domain.ContentHash(nil), snapshotHaves...)
	r.docHaves = append([]domain.ContentHash(nil), docHaves...)
	return r.wants, nil
}

func (r *lazyPushRemote) Push(_ context.Context, _ string, snapshots []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, _, _ bool) error {
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
			out, err := NewSyncRepoService(counting, remote, nil).Push(context.Background(), inbound.SyncInput{RepoID: repoID})
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
			wantObjectCalls := 0
			if test.wantSnapshots > 0 || test.wantDocs > 0 {
				wantObjectCalls = 1
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
	_, err := NewSyncRepoService(counting, remote, nil).Push(context.Background(), inbound.SyncInput{RepoID: repoID})
	if !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("push error=%v, want hash mismatch", err)
	}
	if len(counting.reads) != 0 || remote.objectCalls != 0 || remote.refCalls != 0 {
		t.Fatalf("untrusted wants caused reads/publication: reads=%v objects=%d refs=%d", counting.reads, remote.objectCalls, remote.refCalls)
	}
}
