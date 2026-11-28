package backendclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestReportWorktreesSendsNamesAndBranches(t *testing.T) {
	repo := string(domain.HashContent([]byte("synthetic repository")))
	var method, path, auth string
	var sent map[string]any
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		sent = nil
		_ = json.Unmarshal(raw, &sent)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`[]`))
		} else {
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"forbidden"}}`))
		}
	}))
	defer server.Close()
	client := NewBackendClient(func() string { return server.URL + "/api/v1" }, func() string { return "synthetic-token" })
	worktrees := []domain.WorktreeRegistration{{Name: "app", Branch: "main"}, {Name: "app-detached"}}
	if err := client.ReportWorktrees(context.Background(), repo, "laptop", worktrees); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut || path != "/api/v1/repos/"+repo+"/worktrees" || auth != "Bearer synthetic-token" {
		t.Fatalf("request: %s %s auth=%q", method, path, auth)
	}
	want := map[string]any{"machine": "laptop", "worktrees": []any{map[string]any{"name": "app", "branch": "main"}, map[string]any{"name": "app-detached"}}}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %v", sent)
	}
	// No worktree and no label is still a list, so the server can mark what is gone.
	if err := client.ReportWorktrees(context.Background(), repo, "", nil); err != nil || !reflect.DeepEqual(sent, map[string]any{"worktrees": []any{}}) {
		t.Fatalf("an empty list: %v %v", sent, err)
	}
	status = http.StatusForbidden
	if err := client.ReportWorktrees(context.Background(), repo, "laptop", worktrees); err == nil {
		t.Fatal("a refused list was reported as sent")
	}
	if err := client.ReportWorktrees(context.Background(), "not-a-repository", "laptop", worktrees); err == nil {
		t.Fatal("a malformed repository identity was sent")
	}
}
