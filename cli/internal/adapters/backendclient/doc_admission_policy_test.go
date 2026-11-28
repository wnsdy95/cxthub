package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

func p4R1Negotiation() negotiateResp {
	return negotiateResp{
		DocIdentitiesSupported: []domain.DocumentIdentity{domain.DocumentIdentityRootV1},
		AsyncDocsSupported:     true,
		ChunksSupported:        true, BoundedChunksSupported: true,
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
				client := NewBackendClient(func() string { return server.URL }, func() string { return "" })
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
