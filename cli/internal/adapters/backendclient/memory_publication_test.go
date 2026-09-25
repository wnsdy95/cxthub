package backendclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestMemoryArchivePublicationProtocol(t *testing.T) {
	for _, mode := range []string{"envelope only", "chunked", "old server", "wrong acknowledgement", "legacy preparation", "rejected preparation", "cancelled preparation"} {
		t.Run(mode, func(t *testing.T) {
			repo := string(domain.HashContent([]byte("publication-repo")))
			text := "archive"
			if mode != "envelope only" {
				text = strings.Repeat("large archive ", 150000)
			}
			doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex}, Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}}}
			if mode == "envelope only" {
				doc.CIR.Events = nil
			}
			raw, err := domain.CanonicalBytes(doc.CIR)
			if err != nil {
				t.Fatal(err)
			}
			doc.Hash = domain.HashContent(raw)
			snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Grafted: true, GraftParents: []domain.ContentHash{domain.HashContent([]byte("local overlay"))}, GraftSeq: 2}
			root := domain.MemoryDigest{SnapshotID: doc.Hash, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: doc.Hash, Claims: []domain.MemoryClaim{{Kind: "rationale", Text: "Keep history"}}}}}
			hash, err := domain.MemoryDigestHash(root)
			if err != nil {
				t.Fatal(err)
			}
			publications, preparations := 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			chunks := map[domain.ContentHash][]byte{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/push/negotiate"):
					var req negotiateReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					_ = json.NewEncoder(w).Encode(negotiateResp{AsyncDocsSupported: true, PreparedMemoryArchivesSupported: mode != "legacy preparation", ChunksSupported: true, BoundedChunksSupported: true, ChunkFormatsSupported: []string{chunkcas.FormatV2}, ChunkWants: req.ChunkHaves})
				case strings.HasSuffix(r.URL.Path, "/push/chunks"):
					var req chunksReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					for _, ch := range req.Chunks {
						chunks[ch.Hash] = ch.Data
					}
					_, _ = w.Write([]byte(`{}`))
				case strings.HasSuffix(r.URL.Path, "/push/doc-jobs"):
					preparations++
					var req chunkedDocWire
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if req.Hash != doc.Hash || len(req.Chunks) == 0 {
						t.Error("wrong prepared archive")
					}
					for _, h := range req.Chunks {
						if _, ok := chunks[h]; !ok {
							t.Error("preparation before chunk upload")
						}
					}
					state := "completed"
					if mode == "rejected preparation" {
						state = "rejected"
					}
					if mode == "cancelled preparation" {
						state = "waiting"
						cancel()
					}
					_ = json.NewEncoder(w).Encode(docJobStatus{ID: string(doc.Hash), DocHash: doc.Hash, State: state})
				case strings.HasSuffix(r.URL.Path, "/memory-publications"):
					publications++
					if mode == "old server" {
						http.NotFound(w, r)
						return
					}
					var req struct {
						Objects objectsReq          `json:"objects"`
						Memory  domain.MemoryDigest `json:"memory"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if len(req.Objects.Snapshots) != 1 || req.Objects.Snapshots[0].Grafted || len(req.Objects.Snapshots[0].GraftParents) != 0 || req.Memory.ClaimsVersion != 1 {
						t.Errorf("invalid archive wire: %+v", req.Objects.Snapshots)
					}
					if mode == "envelope only" && (len(req.Objects.Docs) != 1 || len(req.Objects.ChunkedDocs) != 0) {
						t.Error("unchunkable envelope not preserved")
					}
					if mode == "chunked" {
						if len(req.Objects.Docs) != 0 || len(req.Objects.ChunkedDocs) != 0 || len(chunks) == 0 || preparations != 1 {
							t.Error("publication repeated a prepared document or bypassed preparation")
						}
					}
					ack := hash
					if mode == "wrong acknowledgement" {
						ack = domain.HashContent([]byte("wrong"))
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"memory_hash": ack})
				default:
					t.Errorf("non-atomic fallback: %s", r.URL.Path)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer srv.Close()
			client := NewBackendClient(func() string { return srv.URL }, func() string { return "test" }, domain.TeamIdentity{})
			err = client.PublishMemoryArchive(ctx, repo, snap, doc, root)
			blocked := mode == "legacy preparation" || mode == "rejected preparation" || mode == "cancelled preparation"
			if mode == "old server" || mode == "wrong acknowledgement" || blocked {
				if err == nil {
					t.Fatal("unsafe success")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			wantPublications := 1
			if blocked {
				wantPublications = 0
			}
			if publications != wantPublications {
				t.Fatalf("publication count=%d", publications)
			}
		})
	}
}
