package backendclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func p4Root(t *testing.T, empty bool) (domain.DocumentRepresentation, map[domain.ContentHash][]byte) {
	t.Helper()
	cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1"}}
	if !empty {
		cir.Events = []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("a", 3*domain.ConversationManifestChunkBytes)}}}}
	}
	m, b, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	return domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}, b
}

func TestP4RootReceipt(t *testing.T) {
	rep, _ := p4Root(t, true)
	repo := domain.HashContent([]byte("root receipt repo"))
	expected, err := domain.RootDocFinalizationID(repo, rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"good", "opaque-id", "stripped", "wrong-scheme", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			receipt := map[string]any{"id": expected, "doc_hash": rep.Hash, "doc_identity": rep.Identity, "state": "completed"}
			if mode == "opaque-id" {
				receipt["id"] = domain.HashContent([]byte("opaque"))
			}
			if mode == "stripped" {
				delete(receipt, "doc_identity")
			}
			if mode == "wrong-scheme" {
				receipt["doc_identity"] = "future"
			}
			raw, _ := json.Marshal(receipt)
			if mode == "duplicate" {
				raw = append([]byte(`{"doc_identity":"",`), raw[1:]...)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write(raw)
			}))
			defer server.Close()
			c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			err := c.finalizeDocument(context.Background(), string(repo), rep)
			if (err == nil) != (mode == "good") {
				t.Fatalf("receipt %s: %v", mode, err)
			}
		})
	}
}

func TestP4RootPeerBeforeWrites(t *testing.T) {
	for _, mode := range []string{"old", "disabled", "not-opted-in", "missing-async", "zero-wants-old-negotiation", "missing-bounded", "missing-chunks", "missing-v2"} {
		t.Run(mode, func(t *testing.T) {
			rep, bodies := p4Root(t, mode == "missing-async")
			repo := string(domain.HashContent([]byte("root peer")))
			writes, reads := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					supported := []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
					required := domain.DocumentIdentityRootV1
					if mode == "old" {
						supported = nil
					}
					if mode == "not-opted-in" {
						required = domain.DocumentIdentityLegacy
					}
					json.NewEncoder(w).Encode(map[string]any{"id": repo, "doc_identities_supported": supported, "required_doc_identity": required, "root_publication_enabled": mode != "disabled"})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/push/negotiate") {
					neg := negotiateResp{ChunksSupported: true, BoundedChunksSupported: true, AsyncDocsSupported: true, RootPublicationEnabled: true, DocIdentitiesSupported: []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, ChunkFormatsSupported: []string{domain.ConversationManifestChunkFormat}}
					switch mode {
					case "disabled":
						neg.DocWants = []domain.ContentHash{rep.Hash}
						neg.RootPublicationEnabled = false
					case "missing-async":
						neg.AsyncDocsSupported = false
					case "zero-wants-old-negotiation":
						neg.DocIdentitiesSupported = nil
					case "missing-bounded":
						neg.BoundedChunksSupported = false
					case "missing-chunks":
						neg.ChunksSupported = false
					case "missing-v2":
						neg.ChunkFormatsSupported = nil
					}
					json.NewEncoder(w).Encode(neg)
					return
				}
				writes++
				http.Error(w, "unexpected write", 500)
			}))
			defer server.Close()
			c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			ok, err := c.PushDocChunks(context.Background(), repo, outbound.DocumentChunks{Representation: rep, ReadChunk: func(_ context.Context, h domain.ContentHash) ([]byte, error) { reads++; return bodies[h], nil }})
			if !ok || err == nil || writes != 0 || reads != 0 {
				t.Fatalf("old peer fallback/effects: ok=%v err=%v writes=%d reads=%d", ok, err, writes, reads)
			}
		})
	}
}

func TestP4RootReceiptPollingDrift(t *testing.T) {
	rep, _ := p4Root(t, true)
	repo := domain.HashContent([]byte(t.Name()))
	id, err := domain.RootDocFinalizationID(repo, rep)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := docJobStatus{ID: id, DocHash: rep.Hash, DocIdentity: rep.Identity, State: "waiting"}
		if r.Method == http.MethodGet {
			status.State = "completed"
			status.DocIdentity = domain.DocumentIdentityLegacy
		}
		json.NewEncoder(w).Encode(status)
	}))
	defer server.Close()
	c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
	if err := c.finalizeDocument(context.Background(), repo, rep); err == nil {
		t.Fatal("poll lost exact root identity")
	}
}
