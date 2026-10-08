package app

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// Admission controls new document jobs, not metadata or memory attached to an
// already-owned document. Run the same boundary with the binary release fence
// closed and, after release qualification, with its actual root support.
func TestExistingRootPublicationWithAdmissionOff(t *testing.T) {
	for _, operation := range []string{"metadata", "memory"} {
		for _, state := range []string{"stored", "prepared", "old-peer", "no-opt-in", "corrupt", "missing", "wrong-identity", "full-body", "root-descriptor"} {
			t.Run(operation+"/"+state, func(t *testing.T) {
				dir := t.TempDir()
				st := store.NewFSStore(dir)
				svc := NewService(st, st, nil, gitengine.NewEngine(st), st)
				ctx, repo := systemTestContext(), hh(t.Name())
				bindCommitTestRepo(t, st, repo)
				doc, manifest := seedRootReadDoc(t, dir, st, repo, historyMessage(domain.RoleUser, "preserve existing context"))
				if state != "no-opt-in" {
					if err := st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
						t.Fatal(err)
					}
				}
				if state != "old-peer" {
					ctx = inbound.WithDocumentIdentities(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
				}
				if err := svc.ConfigureConversationRootPublication(false); err != nil || svc.RootPublicationEnabled() {
					t.Fatal("admission must remain off", err)
				}
				// A previous successful proof is deliberately warmed before mutation.
				if _, err := st.ReadVerifiedDoc(ctx, repo, doc.Hash); err != nil {
					t.Fatal(err)
				}
				snap, err := st.GetSnapshot(ctx, repo, doc.Hash)
				if err != nil {
					t.Fatal(err)
				}
				objects := inbound.CommitInput{RepoID: repo, Snapshots: []domain.Snapshot{snap}}
				switch state {
				case "prepared":
					if err := st.DeleteSnapshot(ctx, repo, doc.Hash); err != nil {
						t.Fatal(err)
					}
				case "corrupt":
					if err := os.WriteFile(rootReadObjectPath(dir, repo, "chunks", manifest.Chunks[0].Hash), []byte("corrupt"), 0600); err != nil {
						t.Fatal(err)
					}
				case "missing":
					if err := st.DeleteDoc(ctx, repo, doc.Hash); err != nil {
						t.Fatal(err)
					}
				case "wrong-identity":
					objects.Snapshots[0].DocIdentity = domain.DocumentIdentityLegacy
				case "full-body":
					objects.Docs = []domain.SessionDoc{doc}
				case "root-descriptor":
					raw, err := domain.CanonicalConversationManifest(manifest)
					if err != nil {
						t.Fatal(err)
					}
					objects.ChunkedDocs = []inbound.ChunkedDoc{{Hash: doc.Hash, Identity: doc.Identity, RootManifest: raw}}
				}
				before := p9Tree(t, dir)
				publish := func() error {
					if operation == "metadata" {
						_, err := svc.Commit(ctx, objects)
						return err
					}
					_, err := svc.PublishMemoryArchive(ctx, inbound.MemoryPublication{
						Objects: objects,
						Memory:  domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "independent stored memory"},
					})
					return err
				}
				err = publish()
				if !conversationRootReleaseReady || (state != "stored" && state != "prepared") {
					if err == nil {
						t.Fatal("unsupported or unverified root accepted")
					}
					if !reflect.DeepEqual(before, p9Tree(t, dir)) {
						t.Fatal("rejected publication changed stored state")
					}
					return
				}
				if err != nil {
					t.Fatal("existing root blocked by new-document admission", err)
				}
				got, err := st.GetSnapshot(ctx, repo, doc.Hash)
				if err != nil || got.DocumentRef() != doc.DocumentRef() || (operation == "memory" && got.MemoryHash == "") {
					t.Fatal("publication lost identity or memory", got, err)
				}
				if err := publish(); err != nil {
					t.Fatal("exact replay failed", err)
				}
				// Neither operation may create a new root finalization job.
				rep, _ := rootWorkerFixture(t)
				if _, err := svc.SubmitDocFinalization(ctx, repo, rep); !errors.Is(err, domain.ErrRootPublicationDisabled) {
					t.Fatal("new root admission opened", err)
				}
			})
		}
	}
}
