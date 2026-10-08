package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func identityIngressDoc(t *testing.T, text string) domain.SessionDoc {
	t.Helper()
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}},
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	return domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir}
}

// Use the real router, authorization, application service and FS persistence.
// A rejected publication must leave the existing graph/ref and memory intact.
func TestDocumentIdentityHTTPPublicationIngress(t *testing.T) {
	for _, route := range []string{"/push/objects", "/memory-publications"} {
		for _, mode := range []string{
			"root_document_and_snapshot", "root_document_legacy_snapshot",
			"root_declaration_on_legacy_hash", "root_snapshot_legacy_body",
			"root_snapshot_legacy_chunked_body", "root_snapshot_missing_chunked_body",
			"bodyless_root_with_stored_legacy", "bodyless_root_without_body",
			"legacy_inline", "legacy_bodyless", "legacy_chunked",
		} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				ctx := systemTestContext()
				st := store.NewFSStore(t.TempDir())
				svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
				handler := NewServer(svc, app.NewIdentityService(auth.NewDevVerifier(), st)).Handler()
				ts := httptest.NewServer(handler)
				defer ts.Close()
				var repository domain.Repository
				if code := doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]string{"name": "IdentityIngress"}, &repository); code != http.StatusOK {
					t.Fatalf("repository setup: %d", code)
				}
				repo := domain.HashContent([]byte(t.Name()))
				if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repository.ID, DefaultBranch: "main"}); err != nil {
					t.Fatal(err)
				}
				base := "/api/v1/repos/" + string(repo)
				seed := identityIngressDoc(t, "existing graph and branch target")
				seedSnap := domain.Snapshot{ID: seed.Hash, DocHash: seed.Hash, RepoID: repo, Branch: "main", Message: "existing"}
				if code := doJSON(t, "POST", ts.URL+base+"/push/objects", objectsBody{Docs: []domain.SessionDoc{seed}, Snapshots: []domain.Snapshot{seedSnap}}, nil); code != http.StatusOK {
					t.Fatalf("legacy seed publication: %d", code)
				}
				if code := doJSON(t, "PUT", ts.URL+base+"/refs/branch/main", map[string]any{"target": seed.Hash}, nil); code != http.StatusOK {
					t.Fatalf("legacy seed ref: %d", code)
				}
				beforeSnaps, err := st.ListSnapshots(ctx, repo, "")
				if err != nil {
					t.Fatal(err)
				}
				beforeRefs, err := st.ListRefs(ctx, repo)
				if err != nil {
					t.Fatal(err)
				}

				doc := identityIngressDoc(t, "incoming synthetic document")
				legacyHash := doc.Hash
				manifest, _, err := domain.ConversationManifestForCIR(doc.CIR)
				if err != nil {
					t.Fatal(err)
				}
				root, err := domain.ConversationManifestHash(manifest)
				if err != nil || root == legacyHash {
					t.Fatalf("distinct root fixture: %s %v", root, err)
				}
				rootDoc := domain.SessionDoc{Hash: root, CIR: doc.CIR, Identity: domain.DocumentIdentityRootV1}
				if err := domain.VerifySessionDocIdentity(ctx, rootDoc); err != nil {
					t.Fatalf("valid staged root fixture: %v", err)
				}
				snap := domain.Snapshot{ID: legacyHash, DocHash: legacyHash, RepoID: repo, Branch: "main", Parents: []domain.ContentHash{seed.Hash}, Message: "incoming"}
				bodyless, prestored, chunked := false, false, false
				legacy := mode == "legacy_inline" || mode == "legacy_bodyless" || mode == "legacy_chunked"
				switch mode {
				case "root_document_and_snapshot", "root_document_legacy_snapshot":
					doc = rootDoc
					snap.ID, snap.DocHash = root, root
					if mode == "root_document_and_snapshot" {
						snap.DocIdentity = domain.DocumentIdentityRootV1
					}
				case "root_declaration_on_legacy_hash":
					doc.Identity, snap.DocIdentity = domain.DocumentIdentityRootV1, domain.DocumentIdentityRootV1
				case "root_snapshot_legacy_body":
					snap.DocIdentity = domain.DocumentIdentityRootV1
				case "root_snapshot_legacy_chunked_body", "root_snapshot_missing_chunked_body":
					snap.DocIdentity = domain.DocumentIdentityRootV1
					chunked = true
				case "bodyless_root_with_stored_legacy":
					snap.DocIdentity = domain.DocumentIdentityRootV1
					bodyless, prestored = true, true
				case "bodyless_root_without_body":
					snap.ID, snap.DocHash, snap.DocIdentity = root, root, domain.DocumentIdentityRootV1
					bodyless = true
				case "legacy_bodyless":
					bodyless, prestored = true, true
				case "legacy_chunked":
					chunked = true
				}
				if prestored {
					if code := doJSON(t, "POST", ts.URL+base+"/push/objects", objectsBody{Docs: []domain.SessionDoc{doc}}, nil); code != http.StatusOK {
						t.Fatalf("stage legacy body: %d", code)
					}
				}
				objects := objectsBody{Snapshots: []domain.Snapshot{snap}}
				if chunked {
					raw, err := domain.CanonicalBytes(doc.CIR)
					if err != nil {
						t.Fatal(err)
					}
					plan, ok := domain.PlanDocChunks(raw)
					if !ok {
						t.Fatal("chunked fixture has no plan")
					}
					objects.ChunkedDocs = []inbound.ChunkedDoc{{Hash: doc.Hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}}
					// Missing chunks must not obscure the unsupported identity with
					// a reassembly error. Other cases use the real staged-chunk API.
					if mode != "root_snapshot_missing_chunked_body" {
						var chunks []inbound.ChunkObject
						for _, hash := range plan.Order {
							chunks = append(chunks, inbound.ChunkObject{Hash: hash, Data: plan.Bodies[hash]})
						}
						if code := doJSON(t, "POST", ts.URL+base+"/push/chunks", chunksBody{Chunks: chunks}, nil); code != http.StatusOK {
							t.Fatalf("stage chunks: %d", code)
						}
					}
				} else if !bodyless {
					objects.Docs = []domain.SessionDoc{doc}
				}
				digest := domain.MemoryDigest{SnapshotID: snap.ID, Summary: "memory must not attach to an unsupported identity"}
				var payload any = objects
				if route == "/memory-publications" {
					payload = map[string]any{"objects": objects, "memory": digest}
				}
				wire, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, base+route, bytes.NewReader(wire))
				req.Header.Set("Authorization", "Bearer dev:test@t.io:Test")
				req.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				if legacy {
					if response.Code != http.StatusOK {
						t.Fatalf("legacy publication: %d %s", response.Code, response.Body.String())
					}
					got, err := st.GetSnapshot(ctx, repo, snap.ID)
					if err != nil || got.DocIdentity != domain.DocumentIdentityLegacy || got.DocHash != legacyHash {
						t.Fatalf("legacy snapshot: %+v %v", got, err)
					}
					if route == "/memory-publications" {
						memory, err := svc.GetMemoryDigest(ctx, repo, snap.ID)
						if err != nil || memory.Summary != digest.Summary || got.MemoryHash == "" {
							t.Fatalf("legacy memory publication: %+v %v", memory, err)
						}
					}
					if code := doJSON(t, "PUT", ts.URL+base+"/refs/branch/main", map[string]any{"target": snap.ID, "expected_old": seed.Hash}, nil); code != http.StatusOK {
						t.Fatalf("legacy ref advance: %d", code)
					}
					return
				}
				var failure struct{ Error struct{ Code string } }
				if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
					t.Fatal(err)
				}
				if response.Code != http.StatusConflict || failure.Error.Code != "unsupported_document_identity" {
					t.Errorf("unsupported identity: got %d %s; want 409 unsupported_document_identity", response.Code, response.Body.String())
				}
				afterSnaps, err := st.ListSnapshots(ctx, repo, "")
				if err != nil || !reflect.DeepEqual(beforeSnaps, afterSnaps) {
					t.Errorf("rejected publication changed snapshot metadata: before=%+v after=%+v err=%v", beforeSnaps, afterSnaps, err)
				}
				afterRefs, err := st.ListRefs(ctx, repo)
				if err != nil || !reflect.DeepEqual(beforeRefs, afterRefs) {
					t.Errorf("rejected publication changed refs: before=%+v after=%+v err=%v", beforeRefs, afterRefs, err)
				}
				if _, err := st.GetMemoryMeta(ctx, repo, snap.ID); !errors.Is(err, domain.ErrNotFound) {
					t.Errorf("unsupported identity acquired memory metadata: %v", err)
				}
				if !prestored {
					if _, err := st.GetDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
						t.Errorf("rejected publication persisted a document: %v", err)
					}
				}
			})
		}
	}
}

// Keep the hash, snapshot discriminator and chunk bytes valid for legacy
// publication. Only the chunked document's wire declaration changes: losing
// that field in HTTP decoding would otherwise publish the document or job.
func TestDocumentIdentityChunkedHTTPTagPreservation(t *testing.T) {
	for _, format := range []string{domain.ChunkFormatV1, domain.ChunkFormatV2} {
		for _, route := range []string{"/push/objects", "/memory-publications", "/push/doc-jobs"} {
			for _, declaration := range []string{"root_identity", "root_manifest", "legacy"} {
				t.Run(format+route+"/"+declaration, func(t *testing.T) {
					ctx := systemTestContext()
					st := store.NewFSStore(t.TempDir())
					svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
					handler := NewServer(svc, app.NewIdentityService(auth.NewDevVerifier(), st)).Handler()
					ts := httptest.NewServer(handler)
					defer ts.Close()
					var repository domain.Repository
					if code := doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]string{"name": "ChunkIdentityIngress"}, &repository); code != http.StatusOK {
						t.Fatalf("repository setup: %d", code)
					}
					repo := domain.HashContent([]byte(t.Name()))
					if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repository.ID, DefaultBranch: "main"}); err != nil {
						t.Fatal(err)
					}
					base := "/api/v1/repos/" + string(repo)
					doc := identityIngressDoc(t, "first chunked event")
					doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: "second event distinguishes v1 newline framing from v2"}}})
					raw, err := domain.CanonicalBytes(doc.CIR)
					if err != nil {
						t.Fatal(err)
					}
					doc.Hash = domain.HashContent(raw)
					plan, ok := domain.PlanDocChunks(raw)
					if format == domain.ChunkFormatV1 {
						plan, ok = domain.PlanDocChunksV1(raw)
					}
					if !ok || plan.Manifest.Format != format {
						t.Fatal("chunk fixture format mismatch")
					}
					var chunks []inbound.ChunkObject
					for _, hash := range plan.Order {
						chunks = append(chunks, inbound.ChunkObject{Hash: hash, Data: plan.Bodies[hash]})
					}
					if code := doJSON(t, "POST", ts.URL+base+"/push/chunks", chunksBody{Chunks: chunks}, nil); code != http.StatusOK {
						t.Fatalf("stage %s chunks: %d", format, code)
					}
					chunked := inbound.ChunkedDoc{Hash: doc.Hash, Format: format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}
					if declaration == "root_identity" {
						chunked.Identity = domain.DocumentIdentityRootV1
					}
					encoded, err := json.Marshal(chunked)
					if err != nil {
						t.Fatal(err)
					}
					var wire map[string]any
					if err := json.Unmarshal(encoded, &wire); err != nil {
						t.Fatal(err)
					}
					if declaration == "root_manifest" {
						wire["root_manifest"] = map[string]any{}
					}
					snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main"}
					objects := map[string]any{"snapshots": []domain.Snapshot{snap}, "chunked_docs": []any{wire}}
					var payload any = objects
					if route == "/memory-publications" {
						payload = map[string]any{"objects": objects, "memory": domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "chunk identity control"}}
					} else if route == "/push/doc-jobs" {
						payload = wire
					}
					body, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					req := httptest.NewRequest(http.MethodPost, base+route, bytes.NewReader(body))
					req.Header.Set("Authorization", "Bearer dev:test@t.io:Test")
					req.Header.Set("Content-Type", "application/json")
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, req)
					job, err := domain.NewDocFinalizationJob(repo, doc.Hash, plan.Manifest, time.Now().UTC())
					if err != nil {
						t.Fatal(err)
					}
					if declaration == "legacy" {
						want := http.StatusOK
						if route == "/push/doc-jobs" {
							want = http.StatusAccepted
						}
						if response.Code != want {
							t.Fatalf("legacy %s: %d %s", format, response.Code, response.Body.String())
						}
						if route == "/push/doc-jobs" {
							accepted, err := st.GetDocJob(ctx, repo, job.ID)
							if err != nil || accepted.State != "waiting" {
								t.Fatalf("legacy job not queued: %+v %v", accepted, err)
							}
							if err := svc.ProcessDocFinalizations(ctx, 1); err != nil {
								t.Fatal(err)
							}
						}
						stored, err := st.GetDoc(ctx, repo, doc.Hash)
						if err != nil || stored.Identity != domain.DocumentIdentityLegacy || domain.ValidateSessionDocHash(stored) != nil {
							t.Fatalf("legacy %s did not produce a valid document: %v", format, err)
						}
						if route == "/memory-publications" {
							storedSnap, err := st.GetSnapshot(ctx, repo, snap.ID)
							if err != nil || storedSnap.MemoryHash == "" {
								t.Fatalf("legacy chunked memory not attached: %+v %v", storedSnap, err)
							}
						}
						return
					}
					var failure struct{ Error struct{ Code string } }
					if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
						t.Fatal(err)
					}
					wantStatus, wantCode := http.StatusConflict, "unsupported_document_identity"
					if response.Code != wantStatus || failure.Error.Code != wantCode {
						t.Errorf("lost %s declaration: got %d %s; want %d %s", declaration, response.Code, response.Body.String(), wantStatus, wantCode)
					}
					if _, err := st.GetDoc(ctx, repo, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
						t.Errorf("unsupported chunk identity created a document: %v", err)
					}
					if _, err := st.GetDocJob(ctx, repo, job.ID); !errors.Is(err, domain.ErrNotFound) {
						t.Errorf("unsupported chunk identity created a job: %v", err)
					}
					snaps, err := st.ListSnapshots(ctx, repo, "")
					if err != nil || len(snaps) != 0 {
						t.Errorf("unsupported chunk identity created metadata: %+v %v", snaps, err)
					}
					refs, err := st.ListRefs(ctx, repo)
					if err != nil || len(refs) != 0 {
						t.Errorf("unsupported chunk identity moved refs: %+v %v", refs, err)
					}
				})
			}
		}
	}
}
