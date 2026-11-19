package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestPutAnchoredSessionSendsTheReportUnderItsKey(t *testing.T) {
	repo := string(domain.HashContent([]byte("synthetic repository")))
	var method, path, auth, contentType string
	var sent map[string]any
	status, answer := http.StatusOK, `{"revision":4,"body_hash":"`+string(domain.HashContent([]byte("body")))+`","changed":true}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth, contentType = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		sent = nil
		_ = json.Unmarshal(raw, &sent)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	defer server.Close()
	client := NewBackendClient(func() string { return server.URL + "/api/v1" }, func() string { return "synthetic-token" }, domain.TeamIdentity{})
	upload := domain.AnchoredSessionUpload{Provider: domain.ProviderCodex, SessionID: "0199-thread.id_1", Body: domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.SessionScopeFolder, Records: 2,
		Segments: []domain.AnchoredSegment{{AnchoredRange: domain.AnchoredRange{First: "t1/i1", Last: "t1/i2", LastOrdinal: 1},
			Commits: []domain.AnchoredCommit{{SHA: strings.Repeat("a", 40), Rule: domain.SegmentLinkPrintedNumber}}}},
	}}
	receipt, err := client.PutAnchoredSession(context.Background(), repo, upload)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Revision != 4 || !receipt.Changed || receipt.BodyHash != domain.HashContent([]byte("body")) {
		t.Fatalf("receipt: %+v", receipt)
	}
	if method != http.MethodPut || path != "/api/v1/repos/"+repo+"/anchored-sessions/codex/0199-thread.id_1" ||
		auth != "Bearer synthetic-token" || contentType != "application/json" {
		t.Fatalf("request: %s %s auth=%q type=%q", method, path, auth, contentType)
	}
	// The body is the report alone: the key and the uploader are not in it.
	if sent["rule_version"] != float64(1) || sent["scope"] != "working-folder" || sent["records"] != float64(2) || len(sent["segments"].([]any)) != 1 {
		t.Fatalf("body: %v", sent)
	}
	for _, field := range []string{"provider", "session_id", "uploader", "repo_id"} {
		if _, ok := sent[field]; ok {
			t.Fatalf("body carries %s: %v", field, sent)
		}
	}

	// A refusal keeps its status, so the caller can tell a refused report from a refused caller.
	status, answer = http.StatusConflict, `{"error":{"code":"conflict","message":"conflict"}}`
	_, err = client.PutAnchoredSession(context.Background(), repo, upload)
	var refused interface{ StatusCode() int }
	if !errors.As(err, &refused) || refused.StatusCode() != http.StatusConflict {
		t.Fatalf("refusal: %v", err)
	}
	// A repository identifier that is not one never reaches the server.
	method = ""
	if _, err := client.PutAnchoredSession(context.Background(), "not-a-repository", upload); err == nil || method != "" {
		t.Fatalf("invalid repository: %v (request made: %v)", err, method != "")
	}
}
