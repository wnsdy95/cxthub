package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestEffectiveMemoryStaleCursorErrorContract(t *testing.T) {
	const oldMessage = "conflict: effective memory changed; restart without cursor"
	for _, tc := range []struct {
		name, code, message, cursor string
		status                      int
		stale                       bool
	}{
		{"dedicated", "memory_cursor_stale", "generation changed", "page-two", http.StatusConflict, true},
		// Classification is separate from retry policy: callers must reject a
		// stale-cursor response to an initial request without a cursor.
		{"dedicated_without_cursor", "memory_cursor_stale", "generation changed", "", http.StatusConflict, true},
		{"old_server", "conflict", oldMessage, "page-two", http.StatusConflict, false},
		{"other_conflict", "ref_conflict", "memory_cursor_stale", "page-two", http.StatusConflict, false},
		{"missing_code", "", oldMessage, "page-two", http.StatusConflict, false},
		{"unauthorized", "memory_cursor_stale", oldMessage, "page-two", http.StatusUnauthorized, false},
		{"forbidden", "memory_cursor_stale", oldMessage, "page-two", http.StatusForbidden, false},
		{"malformed", "memory_cursor_stale", oldMessage, "page-two", http.StatusUnprocessableEntity, false},
		{"unavailable", "memory_cursor_stale", oldMessage, "page-two", http.StatusServiceUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash := domain.HashContent([]byte("cursor contract"))
			request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: hash, CodeCommit: strings.Repeat("a", 40)}, Content: "prompt", Limit: 50, Cursor: tc.cursor}
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Query().Get("cursor") != tc.cursor {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.code == "" {
					_, _ = w.Write([]byte(tc.message))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": tc.code, "message": tc.message}})
			}))
			defer ts.Close()
			client := NewBackendClient(func() string { return ts.URL }, func() string { return "test-token" }, domain.TeamIdentity{})
			_, err := client.QueryEffectiveMemory(context.Background(), string(hash), request)
			if errors.Is(err, domain.ErrEffectiveMemoryCursorStale) != tc.stale {
				t.Fatalf("stale classification = %v, want %t", err, tc.stale)
			}
			var responseErr *HTTPError
			if !errors.As(err, &responseErr) || responseErr.Status != tc.status || responseErr.Code != tc.code || responseErr.Message != tc.message {
				t.Fatalf("original HTTP error lost: %+v (%v)", responseErr, err)
			}
			if !errors.Is(err, responseErr) || responseErr.Operation != "GET /repos/"+string(hash)+"/effective-memory" {
				t.Fatalf("HTTP error chain or operation lost: %v", err)
			}
			if calls != 1 {
				t.Fatalf("adapter retried %d times", calls)
			}
		})
	}
}

func TestEffectiveMemoryBoundedReadTransport(t *testing.T) {
	for _, mode := range []string{"success", "oversized", "old server"} {
		t.Run(mode, func(t *testing.T) {
			hash := domain.HashContent([]byte("selection"))
			q := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: hash, CodeCommit: strings.Repeat("a", 40), MemoryHash: hash}, Content: "claims", Limit: 50, Cursor: "opaque+cursor/="}
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer test-token" || r.URL.Query().Get("cursor") != q.Cursor || r.URL.Query().Get("content") != "claims" || r.URL.Query().Get("memory_hash") != string(hash) || r.URL.Query().Get("code_commit") != q.Selection.CodeCommit {
					t.Error("wrong request", r.Method, r.URL)
				}
				if mode == "old server" {
					http.NotFound(w, r)
					return
				}
				if mode == "oversized" {
					w.Write([]byte(strings.Repeat(" ", 321<<10)))
					return
				}
				json.NewEncoder(w).Encode(domain.EffectiveMemoryPage{Content: "claims", Selection: q.Selection, StateHash: hash, LineageHash: hash, Items: []domain.EffectiveMemoryItem{}})
			}))
			defer ts.Close()
			c := NewBackendClient(func() string { return ts.URL }, func() string { return "test-token" }, domain.TeamIdentity{})
			out, err := c.QueryEffectiveMemory(context.Background(), string(hash), q)
			if mode == "success" {
				if err != nil || out.Selection != q.Selection || out.Content != "claims" {
					t.Fatal(out, err)
				}
			} else if err == nil {
				t.Fatal("unsafe response accepted")
			}
			if calls != 1 {
				t.Fatal("unexpected retry/write", calls)
			}
		})
	}
}
