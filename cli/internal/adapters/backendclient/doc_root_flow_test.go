package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

func p4DistinctRoot(t *testing.T) (domain.DocumentRepresentation, map[domain.ContentHash][]byte) {
	t.Helper()
	cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1"}}
	for i := 0; i < 7; i++ {
		cir.Events = append(cir.Events, domain.Event{Kind: domain.EventMessage, Seq: i, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat(string(rune('a'+i)), domain.ConversationManifestChunkBytes)}}})
	}
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
	return domain.DocumentRepresentation{Hash: h, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}, b
}

func TestP4RootUpload(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(map[bool]string{true: "empty", false: "bounded"}[empty], func(t *testing.T) {
			rep, bodies := p4Root(t, true)
			if !empty {
				rep, bodies = p4DistinctRoot(t)
			}
			repo := domain.HashContent([]byte("upload repo"))
			jobID, err := domain.RootDocFinalizationID(repo, rep)
			if err != nil {
				t.Fatal(err)
			}
			m, _ := rep.ConversationManifest()
			var order []domain.ContentHash
			for _, c := range m.Chunks {
				order = append(order, c.Hash)
			}
			received := map[domain.ContentHash]int{}
			batches, jobs := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet:
					json.NewEncoder(w).Encode(map[string]any{"id": repo, "required_doc_identity": rep.Identity, "doc_identities_supported": []domain.DocumentIdentity{rep.Identity}, "root_publication_enabled": true})
				case strings.HasSuffix(r.URL.Path, "/negotiate"):
					var req negotiateReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					var wants []domain.ContentHash
					if len(req.ChunkHaves) > 0 {
						wants = order
					}
					json.NewEncoder(w).Encode(negotiateResp{DocWants: []domain.ContentHash{rep.Hash}, ChunkWants: wants, ChunksSupported: true, BoundedChunksSupported: true, AsyncDocsSupported: true, RootPublicationEnabled: true, DocIdentitiesSupported: []domain.DocumentIdentity{rep.Identity}, ChunkFormatsSupported: []string{m.ChunkFormat}})
				case strings.HasSuffix(r.URL.Path, "/chunks"):
					var batch chunksReq
					if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
						t.Error(err)
					}
					total := 0
					for _, c := range batch.Chunks {
						total += len(c.Data)
						received[c.Hash]++
						if !bytes.Equal(c.Data, bodies[c.Hash]) {
							t.Error("wrong body")
						}
					}
					if len(batch.Chunks) > maxChunkWireObjects || total > maxChunkWireRawBytes {
						t.Error("unbounded batch")
					}
					batches++
				case strings.HasSuffix(r.URL.Path, "/doc-jobs"):
					var got domain.DocumentRepresentation
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Error(err)
					}
					if got.DocumentRef() != rep.DocumentRef() || !bytes.Equal(got.RootManifest, rep.RootManifest) || len(received) != len(bodies) {
						t.Error("finalized before exact bodies")
					}
					jobs++
					json.NewEncoder(w).Encode(docJobStatus{ID: jobID, DocHash: rep.Hash, DocIdentity: rep.Identity, State: "completed"})
				default:
					t.Error("unexpected endpoint", r.URL.Path)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer server.Close()
			c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			ok, err := c.PushDocChunks(context.Background(), repo, outbound.DocumentChunks{Representation: rep, ReadChunk: func(_ context.Context, h domain.ContentHash) ([]byte, error) { return bodies[h], nil }})
			if !ok || err != nil || jobs != 1 {
				t.Fatalf("upload %v %v jobs=%d", ok, err, jobs)
			}
			if !empty && batches < 2 {
				t.Fatal("fixture did not cross bounded upload batches")
			}
			if empty && batches != 0 {
				t.Fatal("empty root uploaded chunks")
			}
			for _, n := range received {
				if n != 1 {
					t.Fatal("duplicate chunk upload")
				}
			}
		})
	}
}

func TestP4WireLegacyUnknownPreserved(t *testing.T) {
	var rep chunkedDocWire
	raw := `{"hash":"first","HASH":"second","future":{"ignored":true},"chunks":[]}`
	if err := json.Unmarshal([]byte(raw), &rep); err != nil || rep.Hash != "second" {
		t.Fatalf("legacy behavior changed: %#v %v", rep, err)
	}
	root, _ := p4Root(t, true)
	b, _ := json.Marshal(root)
	for _, bad := range [][]byte{append([]byte(`{"identity":"",`), b[1:]...), bytes.Replace(b, []byte(`"root_manifest"`), []byte(`"ROOT_MANIFEST"`), 1)} {
		if err := json.Unmarshal(bad, &rep); err == nil {
			t.Fatal("ambiguous root wire accepted")
		}
	}
}
