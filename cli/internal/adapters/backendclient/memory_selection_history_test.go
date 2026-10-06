package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestMemorySelectionHistoryRouteFencesOldNodes(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusForbidden, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := NewBackendClient(func() string { return "https://fixture.invalid/api/v1" }, func() string { return "synthetic-test-token" }, domain.TeamIdentity{})
			event := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(domain.HashContent([]byte("repo"))), BranchID: "identity", Branch: "main", Kind: "position", Source: domain.HashContent([]byte("b")), Target: domain.HashContent([]byte("b")), MemoryPinned: true, MemoryHash: domain.HashContent([]byte("c")), GitAfter: strings.Repeat("d", 40), WorktreeID: strings.Repeat("e", 32), CreatedAt: time.Unix(1, 0).UTC(), MemorySelectionParent: strings.Repeat("2", 32)}
			calls := 0
			c.httpc.Transport = ackResponseTransportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodPost || req.URL.Path != "/api/v1/repos/"+event.RepoID+"/history/memory-selection" {
					t.Fatalf("unsafe fallback/request: %s %s", req.Method, req.URL.Path)
				}
				var got domain.HistoryEvent
				if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, event) {
					t.Fatal("immutable history payload changed")
				}
				body := `{"id":"` + event.ID + `"}`
				if status != http.StatusOK {
					body = `{"error":{"code":"not_found","message":"unsupported or rejected"}}`
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			err := c.PushHistoryEvent(context.Background(), event)
			if status == http.StatusOK {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var he *HTTPError
				if !errors.As(err, &he) || he.Status != status {
					t.Fatalf("lost rejection: %v", err)
				}
			}
			if calls != 1 {
				t.Fatalf("retried immutable event via another endpoint: %d", calls)
			}
		})
	}
}
func TestMemorySelectionOrdinaryHistoryRouteUnchanged(t *testing.T) {
	c := NewBackendClient(func() string { return "https://fixture.invalid/api/v1" }, func() string { return "" }, domain.TeamIdentity{})
	e := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(domain.HashContent([]byte("repo"))), BranchID: "identity", Branch: "main", Kind: "position", CreatedAt: time.Unix(1, 0).UTC(), BindingParent: strings.Repeat("2", 32)}
	calls := 0
	c.httpc.Transport = ackResponseTransportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Path != "/api/v1/repos/"+e.RepoID+"/history" {
			t.Fatal("ordinary history route changed")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, exists := body["memory_selection_parent"]; exists {
			t.Fatal("legacy payload widened")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	if err := c.PushHistoryEvent(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
