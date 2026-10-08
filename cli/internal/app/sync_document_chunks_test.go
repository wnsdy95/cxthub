package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type chunkPushStore struct {
	*docReadCountingStore
	unsupported bool
	failure     error
	wrongHash   bool
	active      bool
	opened      int
}

func (s *chunkPushStore) WithVerifiedDocChunks(_ context.Context, ref domain.DocumentRef, use func(outbound.DocumentChunks) error) (bool, error) {
	id := ref.Hash
	s.opened++
	if s.failure != nil || s.unsupported {
		return false, s.failure
	}
	if s.wrongHash {
		id = domain.HashContent([]byte("wrong document"))
	}
	s.active = true
	defer func() { s.active = false }()
	return true, use(outbound.DocumentChunks{Representation: domain.DocumentRepresentation{Hash: id, Identity: ref.Identity}})
}

type chunkPushRemote struct {
	*lazyPushRemote
	upload func(context.Context, outbound.DocumentChunks) (bool, error)
	acked  []domain.ContentHash
	calls  int
}

func (r *chunkPushRemote) PushDocChunks(ctx context.Context, _ string, doc outbound.DocumentChunks) (bool, error) {
	r.calls++
	ok, err := r.upload(ctx, doc)
	if ok && err == nil {
		r.acked = append(r.acked, doc.Representation.Hash)
	}
	return ok, err
}

func TestPushChunkedDocumentsBeforeSnapshotsAndRefs(t *testing.T) {
	base, repo, ids := lazyPushFixture(t)
	store := &chunkPushStore{docReadCountingStore: &docReadCountingStore{SessionStore: base}}
	remote := &chunkPushRemote{lazyPushRemote: &lazyPushRemote{wants: outbound.PushObjectWants{Snapshots: ids, Docs: ids}}}
	remote.upload = func(_ context.Context, doc outbound.DocumentChunks) (bool, error) {
		if !store.active || len(remote.objectSnapshots) != 0 || remote.refCalls != 0 || len(remote.acked)+1 != store.opened {
			t.Fatal("document upload escaped its lifetime or publication order")
		}
		return true, nil
	}
	remote.onPush = func(docs []domain.SessionDoc) error {
		if len(docs) != 0 || len(remote.acked) != len(ids) || store.active {
			t.Fatal("metadata published before all document acknowledgements")
		}
		return nil
	}
	if _, err := newTestSyncService(store, remote, nil).Push(context.Background(), inbound.SyncInput{RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	if len(store.reads) != 0 || len(remote.objectSnapshots) != 2 || remote.refCalls != 1 {
		t.Fatalf("reads=%v snapshots=%d refs=%d", store.reads, len(remote.objectSnapshots), remote.refCalls)
	}
}

func TestPushChunkedDocumentFallbackOnlyForUnsupported(t *testing.T) {
	for _, scenario := range []string{"store-port-absent", "remote-port-absent", "legacy-document", "legacy-server"} {
		t.Run(scenario, func(t *testing.T) {
			base, repo, ids := lazyPushFixture(t)
			store := &chunkPushStore{docReadCountingStore: &docReadCountingStore{SessionStore: base}, unsupported: scenario == "legacy-document"}
			remote := &chunkPushRemote{lazyPushRemote: &lazyPushRemote{wants: outbound.PushObjectWants{Snapshots: ids, Docs: ids}}}
			remote.upload = func(context.Context, outbound.DocumentChunks) (bool, error) { return false, nil }
			var source outbound.SessionStore = store
			var peer outbound.RemoteSync = remote
			if scenario == "store-port-absent" {
				source = store.docReadCountingStore
			}
			if scenario == "remote-port-absent" {
				peer = remote.lazyPushRemote
			}
			if _, err := newTestSyncService(source, peer, nil).Push(context.Background(), inbound.SyncInput{RepoID: repo}); err != nil {
				t.Fatal(err)
			}
			if len(store.reads) != 2 || len(remote.objectDocs) != 2 || remote.refCalls != 1 {
				t.Fatalf("legacy path lost: reads=%v docs=%d refs=%d", store.reads, len(remote.objectDocs), remote.refCalls)
			}
			wantCalls := 0
			if scenario == "legacy-server" {
				wantCalls = 2
			}
			if remote.calls != wantCalls {
				t.Fatalf("chunk upload attempts=%d, want %d", remote.calls, wantCalls)
			}
		})
	}
}

func TestPushChunkedFailuresNeverFallBackOrPublish(t *testing.T) {
	for _, scenario := range []string{"verification", "wrong-hash", "transport", "partial-upload", "cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			base, repo, ids := lazyPushFixture(t)
			store := &chunkPushStore{docReadCountingStore: &docReadCountingStore{SessionStore: base}}
			remote := &chunkPushRemote{lazyPushRemote: &lazyPushRemote{wants: outbound.PushObjectWants{Snapshots: ids, Docs: ids}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("not acknowledged")
			remote.upload = func(context.Context, outbound.DocumentChunks) (bool, error) {
				return scenario == "partial-upload", failure
			}
			switch scenario {
			case "verification":
				store.failure = failure
			case "wrong-hash":
				store.wrongHash = true
				failure = domain.ErrHashMismatch
			case "cancellation":
				failure = context.Canceled
				remote.upload = func(context.Context, outbound.DocumentChunks) (bool, error) {
					cancel()
					return true, nil
				}
			}
			_, err := newTestSyncService(store, remote, nil).Push(ctx, inbound.SyncInput{RepoID: repo})
			if !errors.Is(err, failure) {
				t.Fatalf("error=%v, want %v", err, failure)
			}
			if len(store.reads) != 0 || store.opened != 1 || remote.objectCalls != 0 || remote.refCalls != 0 {
				t.Fatalf("failed chunk upload fell back/continued: reads=%v opens=%d objects=%d refs=%d", store.reads, store.opened, remote.objectCalls, remote.refCalls)
			}
		})
	}
}

type chunkPendingRemote struct {
	*retryPendingRemote
	fail bool
}

func (r *chunkPendingRemote) PushDocChunks(_ context.Context, _ string, doc outbound.DocumentChunks) (bool, error) {
	if r.fail {
		return true, errors.New("finalization not acknowledged")
	}
	if r.docs == nil {
		r.docs = make(map[domain.ContentHash]bool)
	}
	r.docs[doc.Representation.Hash] = true
	return true, nil
}

func TestChunkedPendingRetainsPointerUntilDocumentAcknowledged(t *testing.T) {
	base, repo, ids := lazyPushFixture(t)
	ctx := context.Background()
	if err := base.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: ids[0]}); err != nil {
		t.Fatal(err)
	}
	pending := domain.Pending{RepoID: repo, SessionID: "active", Branch: "main", Target: ids[1]}
	if err := base.PutPending(ctx, pending); err != nil {
		t.Fatal(err)
	}
	store := &chunkPushStore{docReadCountingStore: &docReadCountingStore{SessionStore: base}}
	remote := &chunkPendingRemote{retryPendingRemote: &retryPendingRemote{}, fail: true}
	svc := newTestSyncService(store, remote, nil)
	in := inbound.SyncInput{RepoID: repo, PendingSessionID: "active"}
	if _, err := svc.SyncPendings(ctx, in, nil); err == nil {
		t.Fatal("pending upload failure hidden")
	}
	if len(remote.pendings) != 0 || len(remote.pushes) != 0 || len(store.reads) != 0 {
		t.Fatal("unacknowledged document published pending metadata or used legacy body")
	}
	if pendings, err := base.ListPendings(ctx, repo); err != nil || len(pendings) != 1 || pendings[0].Target != pending.Target {
		t.Fatalf("durable pending lost: %v %v", pendings, err)
	}
	remote.fail = false
	if _, err := svc.SyncPendings(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	if len(remote.pendings) != 1 || remote.pendings[0].Target != pending.Target || len(store.reads) != 0 {
		t.Fatalf("retry did not publish after acknowledgement: %v", remote.pendings)
	}
}
