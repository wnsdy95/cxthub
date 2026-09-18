package backendclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

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
