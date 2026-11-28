package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestRegisterBranchIDsReportsNamesAndIDs(t *testing.T) {
	repo := string(domain.HashContent([]byte("synthetic repository")))
	a := strings.Repeat("a", 32)
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
			_, _ = w.Write([]byte(`{"branches":[{"name":"topic","local_id":"` + a + `","ledger_id":"` + strings.Repeat("f", 32) + `","outcome":"joined"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
		}
	}))
	defer server.Close()
	client := NewBackendClient(func() string { return server.URL + "/api/v1" }, func() string { return "synthetic-token" }, domain.TeamIdentity{})
	results, err := client.RegisterBranchIDs(context.Background(), repo, []domain.BranchRegistration{{Name: "topic", LocalID: a}})
	want := []domain.BranchRegistrationResult{{Name: "topic", LocalID: a, LedgerID: strings.Repeat("f", 32), Outcome: "joined"}}
	if err != nil || !reflect.DeepEqual(results, want) {
		t.Fatalf("results: %+v %v", results, err)
	}
	if method != http.MethodPut || path != "/api/v1/repos/"+repo+"/branch-ids" || auth != "Bearer synthetic-token" {
		t.Fatalf("request: %s %s auth=%q", method, path, auth)
	}
	branches, _ := sent["branches"].([]any)
	if len(branches) != 1 || !reflect.DeepEqual(branches[0], map[string]any{"name": "topic", "local_id": a}) || len(sent) != 1 {
		t.Fatalf("body: %v", sent)
	}
	// No branch on origin is still a report: an empty list, never null.
	if _, err := client.RegisterBranchIDs(context.Background(), repo, nil); err != nil || sent["branches"] == nil {
		t.Fatalf("an empty report: %v %v", sent, err)
	}
	// An older server has no such route; the status says so.
	status = http.StatusNotFound
	_, err = client.RegisterBranchIDs(context.Background(), repo, nil)
	var refused interface{ StatusCode() int }
	if !errors.As(err, &refused) || refused.StatusCode() != http.StatusNotFound {
		t.Fatalf("a server without the route: %v", err)
	}
	method = ""
	if _, err := client.RegisterBranchIDs(context.Background(), "not-a-repository", nil); err == nil || method != "" {
		t.Fatalf("invalid repository: %v", err)
	}
}
