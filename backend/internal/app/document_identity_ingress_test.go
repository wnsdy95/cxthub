package app

import (
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestDocumentIdentityBodylessPublicationIngress(t *testing.T) {
	for _, publication := range []string{"commit", "memory"} {
		for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityRootV1, domain.DocumentIdentityLegacy} {
			t.Run(publication+"/"+string(identity), func(t *testing.T) {
				ctx := systemTestContext()
				svc, st := newFsckSvc(t)
				repo := hh(t.Name())
				bindCommitTestRepo(t, st, repo)
				doc := makeCommitDoc(t, "already staged legacy body")
				if _, err := st.PutDoc(ctx, repo, doc); err != nil {
					t.Fatal(err)
				}
				verified, err := domain.VerifySessionDoc(doc)
				if err != nil {
					t.Fatal(err)
				}
				// A real valid legacy proof is available. Declaring a root scheme
				// must be rejected before even asking this reader for that proof.
				reader := &referenceOnlyBlobs{BlobStore: st, proof: verified.Reference()}
				svc.blobs = reader
				snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, DocIdentity: identity, RepoID: repo}
				objects := inbound.CommitInput{RepoID: repo, Snapshots: []domain.Snapshot{snap}}
				if publication == "commit" {
					_, err = svc.Commit(ctx, objects)
				} else {
					_, err = svc.PublishMemoryArchive(ctx, inbound.MemoryPublication{Objects: objects, Memory: domain.MemoryDigest{SnapshotID: snap.ID, Summary: "staged archive"}})
				}
				if identity == domain.DocumentIdentityLegacy {
					if err != nil {
						t.Fatal(err)
					}
					if reader.verified == 0 || reader.decoded != 0 {
						t.Fatalf("legacy stored proof path: verify=%d decode=%d", reader.verified, reader.decoded)
					}
					if _, err := st.GetSnapshot(ctx, repo, snap.ID); err != nil {
						t.Fatal(err)
					}
					return
				}
				if !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
					t.Errorf("bodyless staged identity publication: %v", err)
				}
				if reader.verified != 0 || reader.decoded != 0 {
					t.Errorf("unsupported identity read body: verify=%d decode=%d", reader.verified, reader.decoded)
				}
				if _, err := st.GetSnapshot(ctx, repo, snap.ID); !errors.Is(err, domain.ErrNotFound) {
					t.Errorf("unsupported identity published snapshot: %v", err)
				}
				if _, err := st.GetMemoryMeta(ctx, repo, snap.ID); !errors.Is(err, domain.ErrNotFound) {
					t.Errorf("unsupported identity attached memory: %v", err)
				}
				refs, err := st.ListRefs(ctx, repo)
				if err != nil || len(refs) != 0 {
					t.Errorf("unsupported identity changed refs: %+v %v", refs, err)
				}
			})
		}
	}
}

func TestDocumentIdentityCommitRejectsMixedBatchBeforePublication(t *testing.T) {
	ctx := systemTestContext()
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	bindCommitTestRepo(t, st, repo)
	seed := prSnapshot(t, st, repo, "existing branch")
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: seed}, ""); err != nil {
		t.Fatal(err)
	}
	beforeSnaps, err := st.ListSnapshots(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	beforeRefs, err := st.ListRefs(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	first, second := makeCommitDoc(t, "valid batch prefix"), makeCommitDoc(t, "legacy body with root-declared snapshot")
	_, err = svc.Commit(ctx, inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{first, second}, Snapshots: []domain.Snapshot{
		{ID: first.Hash, DocHash: first.Hash, Parents: []domain.ContentHash{seed}},
		{ID: second.Hash, DocHash: second.Hash, DocIdentity: domain.DocumentIdentityRootV1, Parents: []domain.ContentHash{first.Hash}},
	}})
	if !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Errorf("mixed identity batch: %v", err)
	}
	afterSnaps, err := st.ListSnapshots(ctx, repo, "")
	if err != nil || !reflect.DeepEqual(beforeSnaps, afterSnaps) {
		t.Errorf("mixed batch changed snapshot metadata: before=%+v after=%+v err=%v", beforeSnaps, afterSnaps, err)
	}
	afterRefs, err := st.ListRefs(ctx, repo)
	if err != nil || !reflect.DeepEqual(beforeRefs, afterRefs) {
		t.Errorf("mixed batch changed refs: before=%+v after=%+v err=%v", beforeRefs, afterRefs, err)
	}
	for _, doc := range []domain.SessionDoc{first, second} {
		if _, err := st.GetDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("mixed batch persisted %s before rejecting identity: %v", doc.Hash, err)
		}
	}
}
