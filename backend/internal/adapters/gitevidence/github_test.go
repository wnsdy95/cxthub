package gitevidence

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGitHubSharedCooldownAndPaginatedHeads(t *testing.T) {
	calls := 0
	limited := true
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if limited {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
			return
		}
		if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("per_page") != "100" {
			t.Error("pagination lost")
		}
		json.NewEncoder(w).Encode([]any{map[string]any{"name": "feature/x", "commit": map[string]string{"sha": strings.Repeat("a", 40)}}})
	}))
	defer ts.Close()
	g := NewGitHub()
	g.base = ts.URL
	for i := 0; i < 3; i++ {
		if _, _, err := g.ListGitHeads(context.Background(), "https://github.com/org/project", 2); err == nil {
			t.Fatal("rate limit accepted")
		}
	}
	if calls != 1 {
		t.Fatal("parallel queues bypassed cooldown", calls)
	}
	limited = false
	g.rateMu.Lock()
	g.resumeAt = time.Time{}
	g.rateMu.Unlock()
	heads, more, err := g.ListGitHeads(context.Background(), "https://github.com/org/project", 2)
	if err != nil || more || len(heads) != 1 || heads[0].Ref != "refs/heads/feature/x" {
		t.Fatal(heads, more, err)
	}
}
