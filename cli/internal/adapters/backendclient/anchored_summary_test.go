package backendclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestReadAnchoredMemoryAsksForABranchAndNamesTheFoldersNewestCommits(t *testing.T) {
	repo := string(domain.HashContent([]byte("synthetic repository")))
	first, second := strings.Repeat("a", 40), strings.Repeat("b", 40)
	var method, path string
	var query map[string][]string
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query = r.Method, r.URL.EscapedPath(), r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"branch":"main","head":"` + second + `","walked":7,"summaries":[{"repo_id":"` + repo + `","provider":"claude","session_id":"session-1","commit":"` + first +
			`","body_hash":"` + string(domain.HashContent([]byte("body"))) + `","body":{"summary":"Added the parser.","key_facts":["It reads TOML."],"open_tasks":[]},"segment_no":2,"uploader":"account","uploader_name":"alice"}]}`))
	}))
	defer server.Close()
	client := NewBackendClient(func() string { return server.URL + "/api/v1" }, func() string { return "synthetic-token" })

	memory, err := client.ReadAnchoredMemory(context.Background(), repo, "topic/parser", []string{first, second}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/api/v1/repos/"+repo+"/anchored-memory" ||
		!reflect.DeepEqual(query, map[string][]string{"branch": {"topic/parser"}, "limit": {"5"}, "local": {first, second}}) {
		t.Fatalf("request: %s %s %v", method, path, query)
	}
	want := domain.AnchoredMemory{Branch: "main", Head: second, Summaries: []domain.AnchoredSummary{{Provider: domain.ProviderClaude, SessionID: "session-1", Commit: first,
		Body: domain.AnchoredSummaryBody{Summary: "Added the parser.", KeyFacts: []string{"It reads TOML."}, OpenTasks: []string{}}, SegmentNo: 2, UploaderName: "alice"}}}
	if !reflect.DeepEqual(memory, want) {
		t.Fatalf("answer:\n got %+v\nwant %+v", memory, want)
	}

	// No branch and no commit: only the limit is sent. More commits than the
	// server takes are cut to the newest.
	if _, err := client.ReadAnchoredMemory(context.Background(), repo, "", nil, 1); err != nil || !reflect.DeepEqual(query, map[string][]string{"limit": {"1"}}) {
		t.Fatalf("without a branch: %v %v", err, query)
	}
	many := make([]string, domain.AnchoredMemoryLocalCommits+3)
	for index := range many {
		many[index] = first
	}
	if _, err := client.ReadAnchoredMemory(context.Background(), repo, "main", many, 5); err != nil || len(query["local"]) != domain.AnchoredMemoryLocalCommits {
		t.Fatalf("many commits: %v %d", err, len(query["local"]))
	}

	// What cannot be a request is not sent.
	method = ""
	if _, err := client.ReadAnchoredMemory(context.Background(), repo, "main", []string{"abc1234"}, 5); !errors.Is(err, domain.ErrInvalidRef) || method != "" {
		t.Fatalf("a short commit number: %v %q", err, method)
	}
	if _, err := client.ReadAnchoredMemory(context.Background(), "not-a-repository", "main", nil, 5); err == nil || method != "" {
		t.Fatalf("a repository ID that is not one: %v %q", err, method)
	}
	status = http.StatusForbidden
	if _, err := client.ReadAnchoredMemory(context.Background(), repo, "main", nil, 5); err == nil {
		t.Fatal("a refusal was read as an answer")
	}
}
