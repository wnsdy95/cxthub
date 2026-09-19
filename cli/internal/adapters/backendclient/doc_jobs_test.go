package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestDocumentFinalizationCancellationRetainsDurableIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hash := domain.HashContent([]byte("doc"))
	id := string(domain.HashContent([]byte("job")))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(docJobStatus{ID: id, DocHash: hash, State: "running"})
		w.(http.Flusher).Flush()
		cancel()
	}))
	defer server.Close()
	c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
	err := c.finalizeDocument(ctx, string(hash), chunkedDocWire{Hash: hash})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled %v", err)
	}
	// Cancellation can race receipt decoding. If receipt arrived, it must carry the
	// durable ID; otherwise the deterministic POST is safely replayed on retry.
	if strings.Contains(err.Error(), "remains durably") && !strings.Contains(err.Error(), id) {
		t.Fatalf("lost job ID %v", err)
	}
}
func TestDocumentFinalizationReceiptIdentityAndRejection(t *testing.T) {
	hash := domain.HashContent([]byte("doc"))
	id := string(domain.HashContent([]byte("job")))
	for _, tc := range []struct {
		name    string
		reply   docJobStatus
		wantErr bool
	}{
		{"completed", docJobStatus{ID: id, DocHash: hash, State: "completed"}, false},
		{"wrong doc", docJobStatus{ID: id, DocHash: domain.HashContent([]byte("wrong")), State: "completed"}, true},
		{"rejected", docJobStatus{ID: id, DocHash: hash, State: "rejected", Reason: "invalid_document"}, true},
		{"unknown state", docJobStatus{ID: id, DocHash: hash, State: "unknown"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(tc.reply)
			}))
			defer server.Close()
			c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			err := c.finalizeDocument(context.Background(), string(hash), chunkedDocWire{Hash: hash})
			if (err != nil) != tc.wantErr {
				t.Fatalf("result %v", err)
			}
		})
	}
}

func TestAsyncDocumentCompletesBeforeSnapshotPublication(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			repo := string(domain.HashContent([]byte("repo")))
			cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1"}, Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "queued body"}}}}}
			cb, _ := domain.CanonicalBytes(cir)
			hash := domain.HashContent(cb)
			doc := domain.SessionDoc{Hash: hash, CIR: cir}
			snap := domain.Snapshot{ID: hash, DocHash: hash, RepoID: repo, Branch: "main"}
			id := string(domain.HashContent([]byte("accepted")))
			var accepted, completed atomic.Bool
			var publications atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/push/negotiate"):
					_ = json.NewEncoder(w).Encode(negotiateResp{SnapshotWants: []domain.ContentHash{hash}, DocWants: []domain.ContentHash{hash}, ChunksSupported: true, BoundedChunksSupported: true, AsyncDocsSupported: true, ChunkFormatsSupported: []string{"cxt-doc-chunks-v2"}})
				case strings.HasSuffix(r.URL.Path, "/push/doc-jobs"):
					accepted.Store(true)
					state := "waiting"
					if reject {
						state = "rejected"
					}
					w.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(w).Encode(docJobStatus{ID: id, DocHash: hash, State: state})
				case strings.HasSuffix(r.URL.Path, "/push/doc-jobs/"+id):
					if !accepted.Load() {
						t.Error("polled before acceptance")
					}
					completed.Store(true)
					_ = json.NewEncoder(w).Encode(docJobStatus{ID: id, DocHash: hash, State: "completed"})
				case strings.HasSuffix(r.URL.Path, "/push/objects"):
					if !completed.Load() {
						t.Error("metadata published before verified receipt")
					}
					var body objectsReq
					_ = json.NewDecoder(r.Body).Decode(&body)
					if len(body.Docs) != 0 || len(body.ChunkedDocs) != 0 || len(body.Snapshots) != 1 {
						t.Errorf("unexpected publication %+v", body)
					}
					publications.Add(1)
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			err := c.Push(context.Background(), repo, []domain.Snapshot{snap}, []domain.SessionDoc{doc}, nil, false, false)
			if reject {
				if err == nil || publications.Load() != 0 {
					t.Fatalf("rejected job published: %d %v", publications.Load(), err)
				}
			} else if err != nil || publications.Load() != 1 {
				t.Fatalf("completion not published: %d %v", publications.Load(), err)
			}
		})
	}
}
