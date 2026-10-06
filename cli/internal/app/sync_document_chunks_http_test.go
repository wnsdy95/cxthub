package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func storedUploadFixture(t testing.TB, size int) (*storage.FileStore, domain.ContentHash, []byte, chunkcas.Plan) {
	t.Helper()
	root := t.TempDir()
	store := storage.NewFileStore(root)
	store.EnableDocVerificationCache(filepath.Join(t.TempDir(), "private", "key"))
	cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex}}
	for i := 0; i < size/(16<<10); i++ {
		cir.Events = append(cir.Events, domain.Event{Seq: i, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("event %d ", i) + strings.Repeat("synthetic body ", 1200)}}})
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.PutDoc(context.Background(), domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := chunkcas.PlanDoc(raw)
	if !ok || len(plan.Order) < 2 {
		t.Fatal("fixture requires multiple chunks")
	}
	if err := store.VerifyStoredDoc(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".cxt", "doc-verification", strings.TrimPrefix(string(id), "sha256:")+".json")); err != nil {
		t.Fatalf("warm upload fixture did not create a verification receipt: %v", err)
	}
	return store, id, raw, plan
}

// Exercise the actual storage and HTTP adapters together. This test server is
// intentionally small: real server finalization is covered by sync E2E.
func TestStoredChunkUploadPreservesCanonicalIdentity(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async=%v", async), func(t *testing.T) {
			store, id, raw, plan := storedUploadFixture(t, 2<<20)
			missing := plan.Order[len(plan.Order)-1]
			var chunksRead, uploaded, finalized atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/push/negotiate"):
					_ = json.NewEncoder(w).Encode(map[string]any{
						"doc_wants": []domain.ContentHash{id}, "chunk_wants": []domain.ContentHash{missing},
						"chunks_supported": true, "bounded_chunks_supported": true,
						"chunk_formats_supported": []string{chunkcas.FormatV2}, "async_docs_supported": async,
					})
				case strings.HasSuffix(r.URL.Path, "/push/chunks"):
					var in struct {
						Chunks []struct {
							Hash domain.ContentHash `json:"hash"`
							Data []byte             `json:"data"`
						} `json:"chunks"`
					}
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Chunks) != 1 {
						t.Errorf("unexpected chunk upload: %+v %v", in, err)
						http.Error(w, "invalid upload", 422)
						return
					}
					if in.Chunks[0].Hash != missing || !bytes.Equal(in.Chunks[0].Data, plan.Bodies[missing]) {
						t.Error("retransmitted existing chunks or changed missing bytes")
					}
					uploaded.Add(1)
					_, _ = w.Write([]byte(`{}`))
				case strings.HasSuffix(r.URL.Path, "/push/objects"), strings.HasSuffix(r.URL.Path, "/push/doc-jobs"):
					var wire struct {
						Hash domain.ContentHash `json:"hash"`
						chunkcas.Manifest
					}
					if async {
						if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
							t.Error(err)
						}
					} else {
						var in struct {
							ChunkedDocs []json.RawMessage `json:"chunked_docs"`
							Snapshots   []json.RawMessage `json:"snapshots"`
							Docs        []json.RawMessage `json:"docs"`
						}
						if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.ChunkedDocs) != 1 || len(in.Snapshots) != 0 || len(in.Docs) != 0 {
							t.Errorf("not a manifest-only publication: %+v %v", in, err)
							http.Error(w, "invalid publication", 422)
							return
						}
						if err := json.Unmarshal(in.ChunkedDocs[0], &wire); err != nil {
							t.Error(err)
						}
					}
					bodies := make([][]byte, len(wire.Chunks))
					for i, hash := range wire.Chunks {
						bodies[i] = plan.Bodies[hash]
					}
					assembled, err := chunkcas.AssembleChunks(wire.Manifest, bodies, wire.Hash)
					if err != nil || wire.Hash != id || !bytes.Equal(raw, assembled) || uploaded.Load() != 1 {
						t.Errorf("stored representation changed: id=%s assembled=%d err=%v", wire.Hash, len(assembled), err)
					}
					finalized.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": domain.HashContent([]byte("job")), "doc_hash": id, "state": "completed"})
				default:
					t.Errorf("unexpected publication route: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			remote := backendclient.NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			source := any(store).(outbound.ChunkedDocumentStore)
			ok, err := source.WithVerifiedDocChunks(context.Background(), id, func(doc outbound.DocumentChunks) error {
				read := doc.ReadChunk
				doc.ReadChunk = func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
					chunksRead.Add(1)
					return read(ctx, hash)
				}
				used, err := any(remote).(outbound.ChunkedDocumentPusher).PushDocChunks(context.Background(), string(domain.HashContent([]byte("repo"))), doc)
				if err == nil && !used {
					t.Error("supported peer fell back")
				}
				return err
			})
			if err != nil || !ok || chunksRead.Load() != 1 || finalized.Load() != 1 {
				t.Fatalf("ok=%v reads=%d finalizations=%d err=%v", ok, chunksRead.Load(), finalized.Load(), err)
			}
		})
	}
}

func TestStoredNonportableChunksUseLegacyPlannerBeforeUpload(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := storage.NewFileStore(root)
	run := strings.Repeat("x", chunkcas.MaxPortableManifestChunks+1)
	cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1"}, Events: []domain.Event{{Seq: 0, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: run}}}}}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := chunkcas.PlanDoc(raw)
	if !ok || len(plan.Order) != 1 {
		t.Fatal("fixture requires one standard chunk")
	}
	body := plan.Bodies[plan.Order[0]]
	start := bytes.Index(body, []byte(run))
	if start < 0 {
		t.Fatal("fixture text absent")
	}
	manifest := chunkcas.Manifest{Format: chunkcas.FormatV2, Envelope: plan.Manifest.Envelope}
	add := func(body []byte) {
		hash := domain.HashContent(body)
		if err := store.PutChunk(ctx, hash, body); err != nil {
			t.Fatal(err)
		}
		manifest.Chunks = append(manifest.Chunks, hash)
	}
	add(body[:start])
	add([]byte("x"))
	for range len(run) - 1 {
		manifest.Chunks = append(manifest.Chunks, manifest.Chunks[1])
	}
	add(body[start+len(run):])
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cxt", "objects", "docs", strings.TrimPrefix(string(id), "sha256:")), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	var negotiated, finalized atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/push/negotiate"):
			var in struct {
				Chunks []domain.ContentHash `json:"chunk_haves"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			if len(in.Chunks) != 1 || in.Chunks[0] != plan.Order[0] {
				t.Errorf("nonportable representation reached negotiation: %v", in.Chunks)
			}
			negotiated.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"doc_wants": []domain.ContentHash{id}, "chunk_wants": in.Chunks, "chunks_supported": true, "bounded_chunks_supported": true, "chunk_formats_supported": []string{chunkcas.FormatV2}, "async_docs_supported": true})
		case strings.HasSuffix(r.URL.Path, "/push/chunks"):
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/push/doc-jobs"):
			var in struct {
				Hash domain.ContentHash `json:"hash"`
				chunkcas.Manifest
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			assembled, err := chunkcas.AssembleChunks(in.Manifest, [][]byte{body}, in.Hash)
			if err != nil || !bytes.Equal(assembled, raw) || in.Hash != id || len(in.Chunks) != 1 {
				t.Errorf("fallback changed canonical document: %v", err)
			}
			finalized.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": domain.HashContent([]byte("job")), "doc_hash": id, "state": "completed"})
		default:
			t.Errorf("unexpected publication: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	remote := backendclient.NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
	if err := newTestSyncService(store, remote, nil).pushDocument(ctx, string(domain.HashContent([]byte("repo"))), id); err != nil {
		t.Fatal(err)
	}
	if negotiated.Load() != 1 || finalized.Load() != 1 {
		t.Fatal("fallback did not finish exactly one publication")
	}
}

// Compare client work for the same warm store, peer and one missing tail chunk.
// HTTP acknowledgements are synthetic; this does not measure server indexing.
func BenchmarkStoredDocumentUpload(b *testing.B) {
	for _, size := range []int{1 << 20, 8 << 20} {
		store, id, raw, plan := storedUploadFixture(b, size)
		missing := plan.Order[len(plan.Order)-1]
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if strings.HasSuffix(r.URL.Path, "/push/negotiate") {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"doc_wants": []domain.ContentHash{id}, "chunk_wants": []domain.ContentHash{missing},
					"chunks_supported": true, "bounded_chunks_supported": true,
					"chunk_formats_supported": []string{chunkcas.FormatV2}, "async_docs_supported": true,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": domain.HashContent([]byte("job")), "doc_hash": id, "state": "completed"})
		}))
		remote := backendclient.NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
		for _, modern := range []bool{false, true} {
			b.Run(fmt.Sprintf("%dMiB/chunked=%v", size>>20, modern), func(b *testing.B) {
				var source outbound.SessionStore = store
				if !modern {
					source = struct{ outbound.SessionStore }{store}
				}
				svc := newTestSyncService(source, remote, nil)
				b.ReportAllocs()
				b.SetBytes(int64(len(raw)))
				b.ResetTimer()
				for b.Loop() {
					if err := svc.pushDocument(context.Background(), string(domain.HashContent([]byte("repo"))), id); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
		server.Close()
	}
}
