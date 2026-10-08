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

type p4LegacyReader struct {
	doc   domain.SessionDoc
	calls int
}

func (r *p4LegacyReader) GetDoc(context.Context, domain.ContentHash) (domain.SessionDoc, error) {
	r.calls++
	return r.doc, nil
}

type p4ExplicitReader struct {
	p4LegacyReader
	explicit int
	cancel   context.CancelFunc
}

func (r *p4ExplicitReader) GetDocReference(context.Context, domain.DocumentRef) (domain.SessionDoc, error) {
	r.explicit++
	if r.cancel != nil {
		r.cancel()
	}
	return r.doc, nil
}

func p4AppRoot(t *testing.T) (domain.DocumentRepresentation, map[domain.ContentHash][]byte, domain.SessionDoc) {
	t.Helper()
	cir := pullDoc(t, "synthetic root").CIR
	m, b, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	h, err := domain.ConversationManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	return domain.DocumentRepresentation{Hash: h, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}, b, domain.SessionDoc{Hash: h, Identity: domain.DocumentIdentityRootV1, CIR: cir}
}

func TestP4ReadDocumentReference(t *testing.T) {
	rep, _, root := p4AppRoot(t)
	legacy := pullDoc(t, "legacy")
	t.Run("legacy fallback verifies", func(t *testing.T) {
		r := &p4LegacyReader{doc: legacy}
		if _, err := readDocumentReference(context.Background(), r, legacy.DocumentRef()); err != nil {
			t.Fatal(err)
		}
		r.doc.Hash = domain.HashContent([]byte("wrong"))
		if _, err := readDocumentReference(context.Background(), r, legacy.DocumentRef()); err == nil {
			t.Fatal("unchecked legacy hash")
		}
	})
	t.Run("root needs explicit port", func(t *testing.T) {
		r := &p4LegacyReader{doc: root}
		if _, err := readDocumentReference(context.Background(), r, rep.DocumentRef()); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) || r.calls != 0 {
			t.Fatal(err, r.calls)
		}
	})
	t.Run("explicit only exact reference", func(t *testing.T) {
		r := &p4ExplicitReader{p4LegacyReader: p4LegacyReader{doc: root}}
		if _, err := readDocumentReference(context.Background(), r, rep.DocumentRef()); err != nil || r.explicit != 1 || r.calls != 0 {
			t.Fatal(err)
		}
		r.doc.Identity = domain.DocumentIdentityLegacy
		if _, err := readDocumentReference(context.Background(), r, rep.DocumentRef()); !errors.Is(err, domain.ErrHashMismatch) {
			t.Fatal(err)
		}
	})
	t.Run("cancel after trusted port", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := &p4ExplicitReader{p4LegacyReader: p4LegacyReader{doc: root}, cancel: cancel}
		got, err := readDocumentReference(ctx, r, rep.DocumentRef())
		if !errors.Is(err, context.Canceled) || got.Hash != "" {
			t.Fatal(got.Hash, err)
		}
	})
	t.Run("unknown before calls", func(t *testing.T) {
		r := &p4LegacyReader{doc: root}
		ref := rep.DocumentRef()
		ref.Identity = "future"
		if _, err := readDocumentReference(context.Background(), r, ref); err == nil || r.calls != 0 {
			t.Fatal(err)
		}
	})
}

func TestP4RootReceiverAndBatchAdoption(t *testing.T) {
	ctx := context.Background()
	rep, bodies, _ := p4AppRoot(t)
	repo := domain.HashContent([]byte(t.Name()))
	snap := domain.Snapshot{ID: rep.Hash, DocHash: rep.Hash, DocIdentity: rep.Identity, RepoID: repo, Branch: "main"}
	store := storage.NewFileStore(t.TempDir())
	receiver := &pullDocumentReceiver{store: store}
	if err := receiver.ReceiveRoot(ctx, rep); err == nil || len(receiver.verified) != 0 {
		t.Fatal("unverified root accepted")
	}
	if err := validatePullBatchWithVerified(ctx, store, repo, []domain.Snapshot{snap}, nil, nil, map[domain.DocumentRef]bool{{Hash: rep.Hash}: true}); err == nil {
		t.Fatal("legacy hash proof became root proof")
	}
	for h, b := range bodies {
		if err := store.PutChunk(ctx, h, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := receiver.ReceiveRoot(ctx, rep); err != nil {
		t.Fatal(err)
	}
	if !receiver.verified[rep.DocumentRef()] || receiver.verified[domain.DocumentRef{Hash: rep.Hash}] {
		t.Fatal("wrong verification key")
	}
	if _, err := store.GetSnapshot(ctx, snap.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("receiving body adopted snapshot")
	}
	if err := validatePullBatchWithVerified(ctx, store, repo, []domain.Snapshot{snap}, nil, nil, receiver.verified); err != nil {
		t.Fatal(err)
	}
	bad := snap
	bad.ID = domain.HashContent([]byte("absent"))
	bad.DocHash = bad.ID
	remote := &metadataOnlyProgressRemote{snaps: []domain.Snapshot{snap, bad}, refs: []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: snap.ID}}}
	if _, err := newTestSyncService(store, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repo}); err == nil {
		t.Fatal("bad suffix accepted")
	}
	if _, err := store.GetSnapshot(ctx, snap.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("partial metadata adopted", err)
	}
	remote.snaps = []domain.Snapshot{snap}
	if _, err := newTestSyncService(store, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSnapshot(ctx, snap.ID)
	if err != nil || got.DocumentRef() != rep.DocumentRef() {
		t.Fatal("root identity lost", err)
	}
	changed := snap
	changed.DocIdentity = domain.DocumentIdentityLegacy
	if err := validatePullBatchWithVerified(ctx, store, repo, []domain.Snapshot{changed}, nil, nil, map[domain.DocumentRef]bool{changed.DocumentRef(): true}); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal("existing identity overwritten", err)
	}
}

func TestP4RootPreflightBeforeMetadataNegotiation(t *testing.T) {
	rep, _, _ := p4AppRoot(t)
	repo := domain.HashContent([]byte(t.Name()))
	s := newTestSyncService(storage.NewFileStore(t.TempDir()), &metadataOnlyProgressRemote{}, nil)
	root := domain.Snapshot{ID: rep.Hash, DocHash: rep.Hash, DocIdentity: rep.Identity, RepoID: repo}
	if err := s.preflightSnapshotReferences(context.Background(), repo, []domain.Snapshot{root}); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Fatal(err)
	}
	legacy := root
	legacy.DocIdentity = domain.DocumentIdentityLegacy
	if err := s.preflightSnapshotReferences(context.Background(), repo, []domain.Snapshot{legacy}); err != nil {
		t.Fatal(err)
	}
	var _ outbound.DocumentReferenceReader = (*p4ExplicitReader)(nil)
}

func TestP4RootCannotFallBackToLegacyPush(t *testing.T) {
	rep, _, _ := p4AppRoot(t)
	base, repo, _ := lazyPushFixture(t)
	store := &chunkPushStore{docReadCountingStore: &docReadCountingStore{SessionStore: base}}
	remote := &chunkPushRemote{lazyPushRemote: &lazyPushRemote{}}
	remote.upload = func(context.Context, outbound.DocumentChunks) (bool, error) { return false, nil }
	svc := newTestSyncService(store, remote, nil)
	if err := svc.pushDocument(context.Background(), repo, rep.DocumentRef()); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Fatal(err)
	}
	if len(store.reads) != 0 || remote.refCalls != 0 || len(remote.objectSnapshots) != 0 {
		t.Fatal("root fell back to legacy reads/writes")
	}
}
