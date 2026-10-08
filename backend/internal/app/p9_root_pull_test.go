package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type p9RootPullStore struct {
	*store.FSStore
	reads, legacy int
}

func (s *p9RootPullStore) WithinReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	return fn(context.WithValue(ctx, p9ReadBoundaryKey{}, true))
}
func (s *p9RootPullStore) WithinRepository(context.Context, domain.ContentHash, func(context.Context) error) error {
	panic("root pull attempted write")
}
func (s *p9RootPullStore) ReadVerifiedDoc(ctx context.Context, r, h domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	if ctx.Value(p9ReadBoundaryKey{}) != true {
		panic("proof escaped snapshot")
	}
	s.reads++
	return s.FSStore.ReadVerifiedDoc(ctx, r, h)
}
func (s *p9RootPullStore) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	s.legacy++
	return domain.DocChunkManifest{}, errors.New("legacy fallback")
}
func (s *p9RootPullStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	s.legacy++
	return domain.SessionDoc{}, errors.New("full fallback")
}
func TestP9PublishedRootPullIsExplicitCurrentProof(t *testing.T) {
	for _, mode := range []string{"valid", "corrupt-chunk", "malformed-manifest", "unpublished", "wrong-identity", "full-doc", "unsupported-cir"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			fs := store.NewFSStore(dir)
			repo := hh(t.Name())
			ctx := context.Background()
			if _, err := fs.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
				t.Fatal(err)
			}
			doc, manifest := seedRootReadDoc(t, dir, fs, repo, historyMessage(domain.RoleUser, "current bytes"))
			if err := fs.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
				t.Fatal(err)
			}
			st := &p9RootPullStore{FSStore: fs}
			svc := NewService(st, st, nil, nil, nil)
			ready := []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
			ctx = outbound.WithDocumentIdentityCompatibility(ctx, ready, ready)
			req := inbound.PullSendInput{RepoID: repo, DocManifestWants: []domain.ContentHash{doc.Hash}, CIRVersionsSupported: []string{"1"}}
			// Warm then mutate current synthetic storage; receipts/caches cannot authorize it.
			if _, err := fs.ReadVerifiedDoc(ctx, repo, doc.Hash); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "corrupt-chunk":
				if err := os.WriteFile(rootReadObjectPath(dir, repo, "chunks", manifest.Chunks[0].Hash), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "malformed-manifest":
				if err := os.WriteFile(rootReadObjectPath(dir, repo, "docs", doc.Hash), []byte(`{"identity":"cxt-manifest-sha256-v1","identity":null}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "unpublished":
				if err := fs.DeleteSnapshot(ctx, repo, doc.Hash); err != nil {
					t.Fatal(err)
				}
			case "wrong-identity":
				snap, err := fs.GetSnapshot(ctx, repo, doc.Hash)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(snap)
				if err != nil {
					t.Fatal(err)
				}
				raw = []byte(strings.Replace(string(raw), string(domain.DocumentIdentityRootV1), "unknown", 1))
				if err := os.WriteFile(filepath.Join(dir, "repos", strings.TrimPrefix(string(repo), "sha256:"), "snapshots", strings.TrimPrefix(string(doc.Hash), "sha256:")), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "full-doc":
				req.DocManifestWants = nil
				req.DocWants = []domain.ContentHash{doc.Hash}
			case "unsupported-cir":
				req.CIRVersionsSupported = []string{"unknown"}
			}
			var out inbound.PullSendOutput
			err := st.WithinReadSnapshot(ctx, func(read context.Context) error { var err error; out, err = svc.send(read, req); return err })
			if mode == "valid" {
				if err != nil || len(out.DocManifests) != 1 || len(out.Docs) != 0 {
					t.Fatal(out, err)
				}
				raw, err := domain.CanonicalConversationManifest(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if out.DocManifests[0].DocumentRef() != doc.DocumentRef() || string(out.DocManifests[0].RootManifest) != string(raw) {
					t.Fatal("identity/descriptor mismatch")
				}
				wire, err := json.Marshal(out.DocManifests[0])
				if err != nil {
					t.Fatal(err)
				}
				var decoded inbound.ChunkedDoc
				if err = json.Unmarshal(wire, &decoded); err != nil {
					t.Fatal(err)
				}
				if err := svc.verifyStoredSnapshotReference(ctx, repo, domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, DocIdentity: doc.Identity}); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid root admitted")
			}
			if st.legacy != 0 {
				t.Fatal("root used legacy fallback", st.legacy)
			}
			before := st.reads
			if _, err := svc.Send(context.Background(), req); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || st.reads != before || st.legacy != 0 {
				t.Fatal("old peer reached root bytes", err)
			}
			public, publicErr := svc.Send(inbound.WithDocumentIdentities(context.Background(), ready), req)
			if !hasDocumentIdentity(svc.DocumentIdentitiesSupported(), domain.DocumentIdentityRootV1) {
				if !errors.Is(publicErr, domain.ErrDocumentIdentityUpgradeRequired) || st.reads != before {
					t.Fatal("unsupported binary reached root bytes", publicErr)
				}
			} else if mode == "valid" {
				if publicErr != nil || !reflect.DeepEqual(public, out) || st.reads != before+1 || svc.RootPublicationEnabled() {
					t.Fatal("public root descriptor lost exact current proof with admission off", publicErr)
				}
			} else if publicErr == nil {
				t.Fatal("public boundary admitted invalid root", mode)
			}
			if st.legacy != 0 {
				t.Fatal("public root used legacy fallback")
			}
		})
	}
}

func TestP9RootAdmissionRequiresFlagOptInAndCompatibility(t *testing.T) {
	for _, mode := range []string{"flag-off", "no-opt-in", "old-peer", "old-binary", "ready"} {
		t.Run(mode, func(t *testing.T) {
			svc, st := newFsckSvc(t)
			ctx := systemTestContext()
			repo := hh(t.Name())
			bindCommitTestRepo(t, st, repo)
			rep, bodies := rootWorkerFixture(t)
			if _, _, err := st.PutChunks(ctx, repo, bodies); err != nil {
				t.Fatal(err)
			}
			if mode != "no-opt-in" {
				if err := st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
					t.Fatal(err)
				}
			}
			peer, binary := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
			if mode == "old-peer" {
				peer = nil
			}
			if mode == "old-binary" {
				binary = nil
			}
			ctx = outbound.WithDocumentIdentityCompatibility(ctx, peer, binary)
			got, err := svc.submitDocFinalizationWithPolicy(ctx, repo, rep, mode != "flag-off")
			if mode == "ready" {
				if err != nil || got.State != "waiting" || got.DocIdentity != rep.Identity {
					t.Fatal(got, err)
				}
				return
			}
			if !errors.Is(err, domain.ErrRootPublicationDisabled) && !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatal(err)
			}
			job, err := domain.NewDocFinalizationJobForRepresentation(repo, rep, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = st.GetDocJob(ctx, repo, job.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("denial queued job", err)
			}
		})
	}
}
