package backendclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestReportCollectorSendsThePushesAndWhatWentWrong(t *testing.T) {
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
			_, _ = w.Write([]byte(`{"heard":true}`))
		} else {
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"forbidden"}}`))
		}
	}))
	defer server.Close()
	client := NewBackendClient(func() string { return server.URL + "/api/v1" }, func() string { return "synthetic-token" })
	sha := strings.Repeat("a", 40)
	report := domain.CollectorReport{Machine: "laptop", Version: "1.2.3", RunError: "the upload was refused", Pushes: []domain.RecordedPush{{Branch: "main", SHA: sha, AtSec: 1_900_000_000}}}
	if err := client.ReportCollector(context.Background(), repo, report); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut || path != "/api/v1/repos/"+repo+"/collector" || auth != "Bearer synthetic-token" {
		t.Fatalf("request: %s %s auth=%q", method, path, auth)
	}
	want := map[string]any{"machine": "laptop", "cxt_version": "1.2.3", "run_error": "the upload was refused",
		"pushes": []any{map[string]any{"branch": "main", "sha": sha, "at": float64(1_900_000_000)}}}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %v", sent)
	}
	// A collector with nothing to name still sends a list: the server refuses a report without one.
	if err := client.ReportCollector(context.Background(), repo, domain.CollectorReport{}); err != nil || !reflect.DeepEqual(sent, map[string]any{"pushes": []any{}}) {
		t.Fatalf("a report that names nothing: %v %v", sent, err)
	}
	status = http.StatusForbidden
	if err := client.ReportCollector(context.Background(), repo, report); err == nil {
		t.Fatal("a refused report was reported as sent")
	}
	// What is no repository identity is refused here: no request is made of it.
	status, method = http.StatusOK, ""
	if err := client.ReportCollector(context.Background(), "not-a-repository", report); err == nil || method != "" {
		t.Fatalf("a malformed repository identity was sent (%q): %v", method, err)
	}
}
