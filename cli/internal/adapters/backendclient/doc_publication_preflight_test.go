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

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestRootPublicationPreflightCurrentNegotiation(t *testing.T) {
	rep, _ := p4Root(t, true)
	for _, mode := range []string{"existing-off", "wanted-off", "wanted-enabled", "missing-identity", "duplicate-identity", "unknown-identity", "missing-async", "missing-bounded", "missing-chunks", "missing-v2", "foreign-want", "duplicate-want", "snapshot-want", "chunk-want", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			repo := domain.HashContent([]byte(t.Name()))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var requests, writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method == http.MethodGet {
					// Deliberately stale enabled discovery: current negotiation is authoritative.
					view := p4R1Profile(repo)
					view["root_publication_enabled"] = true
					p4R1JSON(t, w, view)
					if mode == "cancel" {
						cancel()
					}
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/push/negotiate") {
					writes.Add(1)
					http.Error(w, "unexpected effect", 500)
					return
				}
				var req negotiateReq
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if len(req.DocHaves) != 1 || req.DocHaves[0] != rep.Hash || len(req.SnapshotHaves)+len(req.ChunkHaves) != 0 {
					t.Error("wrong root-only inventory", req)
				}
				neg := p4R1Negotiation()
				switch mode {
				case "wanted-off":
					neg.DocWants = []domain.ContentHash{rep.Hash}
				case "wanted-enabled":
					neg.DocWants = []domain.ContentHash{rep.Hash}
					neg.RootPublicationEnabled = true
				case "missing-identity":
					neg.DocIdentitiesSupported = nil
				case "duplicate-identity":
					neg.DocIdentitiesSupported = append(neg.DocIdentitiesSupported, rep.Identity)
				case "unknown-identity":
					// Bypass the typed encoder's own identity validation to exercise
					// the client's handling of a valid JSON response from a hostile peer.
					body, err := json.Marshal(neg)
					if err != nil {
						t.Error(err)
						return
					}
					var wire map[string]any
					if err := json.Unmarshal(body, &wire); err != nil {
						t.Error(err)
						return
					}
					wire["doc_identities_supported"] = []string{string(rep.Identity), "unknown"}
					p4R1JSON(t, w, wire)
					return
				case "missing-async":
					neg.AsyncDocsSupported = false
				case "missing-bounded":
					neg.BoundedChunksSupported = false
				case "missing-chunks":
					neg.ChunksSupported = false
				case "missing-v2":
					neg.ChunkFormatsSupported = nil
				case "foreign-want":
					neg.DocWants = []domain.ContentHash{domain.HashContent([]byte("foreign"))}
				case "duplicate-want":
					neg.DocWants = []domain.ContentHash{rep.Hash, rep.Hash}
				case "snapshot-want":
					neg.SnapshotWants = []domain.ContentHash{rep.Hash}
				case "chunk-want":
					neg.ChunkWants = []domain.ContentHash{rep.Hash}
				}
				p4R1JSON(t, w, neg)
			}))
			defer server.Close()
			c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			err := c.PreflightDocumentReferences(ctx, repo, []domain.DocumentRef{rep.DocumentRef(), rep.DocumentRef()})
			good := mode == "existing-off" || mode == "wanted-enabled"
			if (err == nil) != good || writes.Load() != 0 {
				t.Fatalf("err=%v writes=%d", err, writes.Load())
			}
			if mode == "cancel" && (!errors.Is(err, context.Canceled) || requests.Load() != 1) {
				t.Fatal("cancellation", err, requests.Load())
			}
		})
	}
}

func TestRootPublicationPreflightFreezesPeerAndSkipsLegacy(t *testing.T) {
	rep, _ := p4Root(t, true)
	repo := domain.HashContent([]byte(t.Name()))
	var requests, bases, tokens atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer frozen" {
			t.Error("credentials changed")
		}
		if r.Method == http.MethodGet {
			p4R1JSON(t, w, p4R1Profile(repo))
			return
		}
		p4R1JSON(t, w, p4R1Negotiation())
	}))
	defer server.Close()
	c := NewBackendClient(func() string {
		if bases.Add(1) != 1 {
			return server.URL + "/changed"
		}
		return server.URL
	}, func() string {
		if tokens.Add(1) != 1 {
			return "changed"
		}
		return "frozen"
	}, domain.TeamIdentity{})
	if err := c.PreflightDocumentReferences(context.Background(), repo, []domain.DocumentRef{rep.DocumentRef()}); err != nil {
		t.Fatal(err)
	}
	if bases.Load() != 1 || tokens.Load() != 1 || requests.Load() != 2 {
		t.Fatal("not a frozen pair", bases.Load(), tokens.Load(), requests.Load())
	}
	if err := c.PreflightDocumentReferences(context.Background(), repo, []domain.DocumentRef{{Hash: rep.Hash}}); err != nil {
		t.Fatal(err)
	}
	if bases.Load() != 1 || tokens.Load() != 1 || requests.Load() != 2 {
		t.Fatal("legacy preflight gained network")
	}
}
