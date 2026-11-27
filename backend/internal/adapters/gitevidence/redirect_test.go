package gitevidence

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The client reads GitHub's API with a credential in every request. An answer
// that points somewhere else is not followed: the credential would travel to
// wherever the answer points. The read fails instead.
func TestGitHubDoesNotFollowARedirect(t *testing.T) {
	followed, credential := 0, ""
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed++
		credential = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"name": "main", "commit": map[string]string{"sha": strings.Repeat("a", 40)}}})
	}))
	defer elsewhere.Close()
	asked := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		http.Redirect(w, r, elsewhere.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer server.Close()
	provider := NewGitHub(func() string { return "synthetic-token" })
	provider.base = server.URL
	heads, more, err := provider.ListGitHeads(context.Background(), "https://github.com/org/project", 1)
	if err == nil || len(heads) != 0 || more {
		t.Fatalf("a redirected read answered %v %v %v", heads, more, err)
	}
	if asked != 1 || followed != 0 || credential != "" {
		t.Fatalf("asked %d times, followed the redirect %d times, credential sent elsewhere: %q", asked, followed, credential)
	}
}
