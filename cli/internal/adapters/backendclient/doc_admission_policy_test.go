package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func p4R1Negotiation() negotiateResp {
	return negotiateResp{
		DocIdentitiesSupported: []domain.DocumentIdentity{domain.DocumentIdentityRootV1},
		AsyncDocsSupported:     true, PreparedMemoryArchivesSupported: true,
		ChunksSupported: true, BoundedChunksSupported: true,
		ChunkFormatsSupported:  []string{domain.ConversationManifestChunkFormat},
		RootPublicationEnabled: false,
	}
}

func p4R1Profile(repo string) map[string]any {
	return map[string]any{
		"id": repo, "required_doc_identity": domain.DocumentIdentityRootV1,
		"doc_identities_supported": []domain.DocumentIdentity{domain.DocumentIdentityRootV1},
		"root_publication_enabled": false,
	}
}

func p4R1JSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func TestP4R1MetadataAdmissionOff(t *testing.T) {
	rep, _ := p4Root(t, true)
	for _, mode := range []string{"zero-wants", "snapshot-wanted", "old-peer", "not-opted-in", "wrong-repo"} {
		t.Run(mode, func(t *testing.T) {
			repo := domain.HashContent([]byte(t.Name()))
			snap := domain.Snapshot{ID: rep.Hash, DocHash: rep.Hash, DocIdentity: rep.Identity, RepoID: repo}
			var negotiations, writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet:
					profile := p4R1Profile(repo)
					switch mode {
					case "old-peer":
						delete(profile, "doc_identities_supported")
					case "not-opted-in":
						profile["required_doc_identity"] = domain.DocumentIdentityLegacy
					case "wrong-repo":
						profile["id"] = domain.HashContent([]byte("other repo"))
					}
					p4R1JSON(t, w, profile)
				case strings.HasSuffix(r.URL.Path, "/push/negotiate"):
					negotiations.Add(1)
					neg := p4R1Negotiation()
					var req negotiateReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if mode == "snapshot-wanted" && len(req.SnapshotHaves) > 0 {
						neg.SnapshotWants = []domain.ContentHash{snap.ID}
					}
					p4R1JSON(t, w, neg)
				case strings.HasSuffix(r.URL.Path, "/push/objects") && mode == "snapshot-wanted":
					writes.Add(1)
					var got objectsReq
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Error(err)
					}
					if len(got.Snapshots) != 1 || got.Snapshots[0].DocumentRef() != rep.DocumentRef() || len(got.Docs) != 0 || len(got.ChunkedDocs) != 0 || len(got.ChunkObjects) != 0 {
						t.Error("metadata publication lost identity or included document bodies")
					}
					p4R1JSON(t, w, struct{}{})
				default:
					writes.Add(1)
					http.Error(w, "unexpected write", http.StatusBadRequest)
				}
			}))
			defer server.Close()
			client := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			err := client.Push(context.Background(), repo, []domain.Snapshot{snap}, nil, nil, false, false)
			wantNegotiations, wantWrites := int32(2), int32(0)
			var wantErr error
			switch mode {
			case "snapshot-wanted":
				wantWrites = 1
			case "old-peer", "not-opted-in":
				wantNegotiations, wantErr = 0, domain.ErrUnsupportedDocumentIdentity
			case "wrong-repo":
				wantNegotiations, wantErr = 0, domain.ErrHashMismatch
			}
			if !errors.Is(err, wantErr) || negotiations.Load() != wantNegotiations || writes.Load() != wantWrites {
				t.Fatalf("err=%v want=%v negotiations=%d/%d writes=%d/%d", err, wantErr, negotiations.Load(), wantNegotiations, writes.Load(), wantWrites)
			}
		})
	}
}

func TestP4R1UploadAdmissionOff(t *testing.T) {
	for _, empty := range []bool{true, false} {
		rep, bodies := p4Root(t, empty)
		for _, wanted := range []bool{false, true} {
			name := map[bool]string{true: "empty", false: "events"}[empty] + "/" + map[bool]string{true: "new-root", false: "existing-root"}[wanted]
			t.Run(name, func(t *testing.T) {
				repo := domain.HashContent([]byte(t.Name()))
				var negotiations, reads, writes atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet:
						profile := p4R1Profile(repo)
						// A stale enabled discovery response must not authorize new roots.
						profile["root_publication_enabled"] = wanted
						p4R1JSON(t, w, profile)
					case strings.HasSuffix(r.URL.Path, "/push/negotiate"):
						negotiations.Add(1)
						var req negotiateReq
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							t.Error(err)
						}
						neg := p4R1Negotiation()
						if wanted {
							neg.DocWants, neg.ChunkWants = []domain.ContentHash{rep.Hash}, req.ChunkHaves
						}
						p4R1JSON(t, w, neg)
					default:
						writes.Add(1)
						http.Error(w, "unexpected write", http.StatusBadRequest)
					}
				}))
				defer server.Close()
				client := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
				accepted, err := client.PushDocChunks(context.Background(), repo, outbound.DocumentChunks{Representation: rep, ReadChunk: func(_ context.Context, h domain.ContentHash) ([]byte, error) {
					reads.Add(1)
					return bodies[h], nil
				}})
				var wantErr error
				if wanted {
					wantErr = domain.ErrUnsupportedDocumentIdentity
				}
				wantNegotiations := int32(2)
				if wanted {
					wantNegotiations = 1
				}
				if !accepted || !errors.Is(err, wantErr) || negotiations.Load() != wantNegotiations || reads.Load() != 0 || writes.Load() != 0 {
					t.Fatalf("accepted=%v err=%v want=%v negotiations=%d reads=%d writes=%d", accepted, err, wantErr, negotiations.Load(), reads.Load(), writes.Load())
				}
			})
		}
	}
}

func TestP4R1MemoryArchiveAdmissionOff(t *testing.T) {
	for _, mode := range []string{"existing-empty-root", "existing-root", "new-root", "root-disappears", "corrupt-current-chunk"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			rep, bodies := p4Root(t, mode == "existing-empty-root")
			dir := t.TempDir()
			store := storage.NewFileStore(dir)
			for hash, body := range bodies {
				if err := store.PutChunk(ctx, hash, body); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.PutConversationManifest(ctx, rep); err != nil {
				t.Fatal(err)
			}
			doc, err := store.GetDocReference(ctx, rep.DocumentRef())
			if err != nil {
				t.Fatal(err)
			}
			// Corrupt after successful install/read so cached proof cannot suffice.
			if mode == "corrupt-current-chunk" {
				for hash, body := range bodies {
					path := filepath.Join(dir, ".cxt", "objects", "chunks", strings.TrimPrefix(hash, "sha256:"))
					corrupt := append([]byte(nil), body...)
					corrupt[len(corrupt)/2] ^= 1
					if err := os.WriteFile(path, corrupt, 0600); err != nil {
						t.Fatal(err)
					}
					break
				}
			}
			repo := domain.HashContent([]byte(t.Name()))
			snap := domain.Snapshot{ID: rep.Hash, DocHash: rep.Hash, DocIdentity: rep.Identity, RepoID: repo}
			memory := domain.MemoryDigest{SnapshotID: snap.ID, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: snap.ID, Claims: []domain.MemoryClaim{{Kind: "rationale", Text: "Keep existing root history"}}}}}
			memoryHash, err := domain.MemoryDigestHash(memory)
			if err != nil {
				t.Fatal(err)
			}
			var negotiations, publications, forbiddenWrites atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet:
					p4R1JSON(t, w, p4R1Profile(repo))
				case strings.HasSuffix(r.URL.Path, "/push/negotiate"):
					n := negotiations.Add(1)
					var req negotiateReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if len(req.DocHaves) != 1 || req.DocHaves[0] != rep.Hash {
						t.Error("wrong offered archive root")
					}
					neg := p4R1Negotiation()
					if mode == "new-root" || (mode == "root-disappears" && n == 4) {
						neg.DocWants, neg.ChunkWants = []domain.ContentHash{rep.Hash}, req.ChunkHaves
					}
					p4R1JSON(t, w, neg)
				case strings.HasSuffix(r.URL.Path, "/memory-publications"):
					publications.Add(1)
					var req struct {
						Objects objectsReq          `json:"objects"`
						Memory  domain.MemoryDigest `json:"memory"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if len(req.Objects.Snapshots) != 1 || req.Objects.Snapshots[0].DocumentRef() != rep.DocumentRef() || req.Objects.Snapshots[0].RepoID != repo || len(req.Objects.Docs) != 0 || len(req.Objects.ChunkedDocs) != 0 || len(req.Objects.ChunkObjects) != 0 {
						t.Error("archive publication changed identity or sent document bodies")
					}
					got, err := domain.MemoryDigestHash(req.Memory)
					if err != nil || got != memoryHash {
						t.Error("archive publication changed memory", err)
					}
					p4R1JSON(t, w, map[string]any{"memory_hash": memoryHash})
				default:
					forbiddenWrites.Add(1)
					http.Error(w, "unexpected staging/finalization", http.StatusBadRequest)
				}
			}))
			defer server.Close()
			client := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			client.SetChunkLocal(store)
			err = client.PublishMemoryArchive(ctx, repo, snap, doc, memory)
			wantNegotiations, wantPublications := int32(4), int32(0)
			switch mode {
			case "existing-empty-root", "existing-root":
				wantPublications = 1
				if err != nil {
					t.Errorf("existing root archive rejected: %v", err)
				}
			case "new-root", "root-disappears":
				if mode == "new-root" {
					wantNegotiations = 1
				}
				if !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
					t.Errorf("new root admitted: %v", err)
				}
			case "corrupt-current-chunk":
				wantNegotiations = 2
				if !errors.Is(err, domain.ErrConversationManifest) {
					t.Errorf("current-byte verification did not reject corrupt chunk: %v", err)
				}
			}
			if negotiations.Load() != wantNegotiations || publications.Load() != wantPublications || forbiddenWrites.Load() != 0 {
				t.Fatalf("negotiations=%d/%d publications=%d/%d forbidden writes=%d", negotiations.Load(), wantNegotiations, publications.Load(), wantPublications, forbiddenWrites.Load())
			}
		})
	}
}
