package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

func docUploadFixture(t *testing.T, version string) outbound.DocumentChunks {
	t.Helper()
	raw, err := domain.CanonicalBytes(domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: version},
		Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user",
			Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic upload fixture"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := chunkcas.PlanDoc(raw)
	if !ok {
		t.Fatal("fixture did not produce a manifest")
	}
	return outbound.DocumentChunks{Representation: domain.DocumentRepresentation{Hash: domain.HashContent(raw), Format: plan.Manifest.Format,
		Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks},
		ReadChunk: func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
			body, ok := plan.Bodies[hash]
			if !ok {
				return nil, domain.ErrNotFound
			}
			return body, nil
		}}
}

// Execute handlers synchronously for deterministic loader/HTTP ordering and
// cancellation at precise boundaries, using the client's real JSON transport.
func docUploadClient(t *testing.T, repo string, handle http.HandlerFunc) *BackendClient {
	t.Helper()
	c := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
	c.httpc.Transport = reviewTransferRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost && !(r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/push/doc-jobs/")) {
			t.Fatalf("unexpected method %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.URL.Path, "/repos/"+repo+"/push/") {
			t.Fatalf("unexpected resource %s", r.URL.Path)
		}
		w := httptest.NewRecorder()
		handle(w, r)
		return w.Result(), nil
	})
	return c
}

func TestPushDocChunksValidatesDescriptorBeforeNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*outbound.DocumentChunks, *string)
		want error
	}{
		{"repo", func(_ *outbound.DocumentChunks, r *string) { *r = "invalid" }, domain.ErrHashMismatch},
		{"document hash", func(d *outbound.DocumentChunks, _ *string) { d.Representation.Hash = "invalid" }, domain.ErrHashMismatch},
		{"format", func(d *outbound.DocumentChunks, _ *string) { d.Representation.Format = "future" }, domain.ErrHashMismatch},
		{"empty chunks", func(d *outbound.DocumentChunks, _ *string) { d.Representation.Chunks = nil }, domain.ErrHashMismatch},
		{"chunk hash", func(d *outbound.DocumentChunks, _ *string) { d.Representation.Chunks[0] = "invalid" }, domain.ErrHashMismatch},
		{"nil loader", func(d *outbound.DocumentChunks, _ *string) { d.ReadChunk = nil }, domain.ErrHashMismatch},
		{"missing envelope", func(d *outbound.DocumentChunks, _ *string) { d.Representation.Envelope = nil }, domain.ErrInvalidCIR},
		{"empty envelope", func(d *outbound.DocumentChunks, _ *string) { d.Representation.Envelope = json.RawMessage(`{}`) }, domain.ErrInvalidCIR},
		{"incomplete envelope", func(d *outbound.DocumentChunks, _ *string) {
			d.Representation.Envelope = json.RawMessage(`{"cir_version":"1"}`)
		}, domain.ErrInvalidCIR},
		{"noncanonical envelope", func(d *outbound.DocumentChunks, _ *string) {
			d.Representation.Envelope = append([]byte(" "), d.Representation.Envelope...)
		}, domain.ErrInvalidCIR},
		{"null envelope", func(d *outbound.DocumentChunks, _ *string) { d.Representation.Envelope = json.RawMessage(`null`) }, domain.ErrInvalidCIR},
		{"array envelope", func(d *outbound.DocumentChunks, _ *string) { d.Representation.Envelope = json.RawMessage(`[]`) }, domain.ErrInvalidCIR},
		{"string envelope", func(d *outbound.DocumentChunks, _ *string) { d.Representation.Envelope = json.RawMessage(`"oops"`) }, domain.ErrInvalidCIR},
		{"malformed envelope", func(d *outbound.DocumentChunks, _ *string) { d.Representation.Envelope = json.RawMessage(`{`) }, domain.ErrInvalidCIR},
		{"malformed version", func(d *outbound.DocumentChunks, _ *string) {
			d.Representation.Envelope = json.RawMessage(`{"cir_version":2}`)
		}, domain.ErrInvalidCIR},
		{"unknown version", func(d *outbound.DocumentChunks, _ *string) {
			d.Representation.Envelope = json.RawMessage(`{"cir_version":"999"}`)
		}, domain.ErrUnsupportedCIRVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := docUploadFixture(t, domain.CIRVersionV1)
			repo := domain.HashContent([]byte("repo"))
			doc.ReadChunk = func(context.Context, domain.ContentHash) ([]byte, error) {
				t.Fatal("invalid descriptor read a chunk")
				return nil, nil
			}
			tc.edit(&doc, &repo)
			c := docUploadClient(t, repo, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid descriptor contacted server") })
			ok, err := c.PushDocChunks(context.Background(), repo, doc)
			if !ok || !errors.Is(err, tc.want) {
				t.Fatalf("handled=%v error=%v, want %v without fallback", ok, err, tc.want)
			}
		})
	}
}

func TestPushDocChunksManifestLimitsFallBackBeforeRequests(t *testing.T) {
	for _, kind := range []string{"ordered-count", "encoded-bytes"} {
		t.Run(kind, func(t *testing.T) {
			doc := docUploadFixture(t, domain.CIRVersionV1)
			if kind == "ordered-count" {
				id := doc.Representation.Chunks[0]
				for len(doc.Representation.Chunks) <= chunkcas.MaxPortableManifestChunks {
					doc.Representation.Chunks = append(doc.Representation.Chunks, id)
				}
			} else {
				raw, err := domain.CanonicalBytes(domain.CIRDocument{Envelope: domain.Envelope{
					CIRVersion: domain.CIRVersionV1, SessionOriginID: strings.Repeat("x", chunkcas.MaxPortableManifestBytes),
				}})
				if err != nil {
					t.Fatal(err)
				}
				var value struct {
					Envelope json.RawMessage `json:"envelope"`
				}
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				doc.Representation.Envelope = value.Envelope
			}
			doc.ReadChunk = func(context.Context, domain.ContentHash) ([]byte, error) {
				t.Error("nonportable manifest read an upload body")
				return nil, errors.New("unexpected body read")
			}
			repo := domain.HashContent([]byte(t.Name()))
			client := docUploadClient(t, repo, func(http.ResponseWriter, *http.Request) {
				t.Error("nonportable manifest contacted server")
			})
			if used, err := client.PushDocChunks(context.Background(), repo, doc); used || err != nil {
				t.Fatalf("clean pre-write fallback: used=%v error=%v", used, err)
			}
		})
	}
}

func TestPushDocChunksMalformedNegotiationNeverFallsBack(t *testing.T) {
	for _, reply := range []string{`{"chunks_supported":`, `{"doc_wants":true}`, `{"bounded_chunks_supported":"yes"}`} {
		t.Run(reply, func(t *testing.T) {
			doc := docUploadFixture(t, "1")
			repo := domain.HashContent([]byte("repo"))
			calls := 0
			c := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if !strings.HasSuffix(r.URL.Path, "/negotiate") {
					t.Fatalf("malformed negotiation caused write %s", r.URL.Path)
				}
				_, _ = io.WriteString(w, reply)
			})
			doc.ReadChunk = func(context.Context, domain.ContentHash) ([]byte, error) {
				t.Fatal("malformed negotiation caused read")
				return nil, nil
			}
			ok, err := c.PushDocChunks(context.Background(), repo, doc)
			if !ok || err == nil || calls != 1 {
				t.Fatalf("handled=%v error=%v calls=%d", ok, err, calls)
			}
		})
	}
}
