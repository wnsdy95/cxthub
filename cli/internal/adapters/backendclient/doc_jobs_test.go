package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
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
	c := NewBackendClient(func() string { return server.URL }, func() string { return "" })
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
			c := NewBackendClient(func() string { return server.URL }, func() string { return "" })
			err := c.finalizeDocument(context.Background(), string(hash), chunkedDocWire{Hash: hash})
			if (err != nil) != tc.wantErr {
				t.Fatalf("result %v", err)
			}
		})
	}
}

// A document the server takes at once is not waited a second for: the server
// is asked soon, and then after twice as long each time.
func TestDocumentFinalizationAsksSoonAndThenLessOften(t *testing.T) {
	hash := domain.HashContent([]byte("doc"))
	id := string(domain.HashContent([]byte("job")))
	var asked []time.Time
	// The job ends by the fourth question.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := "running"
		if r.Method == http.MethodGet {
			if asked = append(asked, time.Now()); len(asked) == 4 {
				state = "completed"
			}
		}
		_ = json.NewEncoder(w).Encode(docJobStatus{ID: id, DocHash: hash, State: state})
	}))
	defer server.Close()
	c := NewBackendClient(func() string { return server.URL }, func() string { return "" })
	began := time.Now()
	if err := c.finalizeDocument(context.Background(), string(hash), chunkedDocWire{Hash: hash}); err != nil {
		t.Fatal(err)
	}
	// 50, 100, 200 and 400 milliseconds: under a second in all, and each wait
	// longer than the one before.
	if len(asked) != 4 || asked[0].Sub(began) < 40*time.Millisecond || asked[3].Sub(began) > 2*time.Second || asked[3].Sub(began) < 700*time.Millisecond {
		t.Fatalf("asked %d times, first after %v, last after %v", len(asked), asked[0].Sub(began), asked[len(asked)-1].Sub(began))
	}
	for index := 2; index < len(asked); index++ {
		if asked[index].Sub(asked[index-1]) <= asked[index-1].Sub(asked[index-2]) {
			t.Fatalf("the wait before question %d is not longer than the one before: %v", index+1, asked)
		}
	}
	if docJobFirstWait != 50*time.Millisecond || docJobLongestWait != 5*time.Second {
		t.Fatalf("waits: %v to %v", docJobFirstWait, docJobLongestWait)
	}
}
