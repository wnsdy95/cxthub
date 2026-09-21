package gitevidence

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSyncAuditProviderRequiresExactPRIdentity(t *testing.T) {
	owner := "org/project"
	number := 7
	merged := true
	list := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("missing auth")
		}
		row := map[string]any{"number": number, "merged": merged, "merged_at": "2026-01-01T00:00:00Z", "merge_commit_sha": strings.Repeat("b", 40), "base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": owner}}, "head": map[string]any{"ref": "topic", "sha": strings.Repeat("a", 40)}}
		if list {
			if r.URL.Query().Get("per_page") != "20" {
				t.Error("unbounded page")
			}
			json.NewEncoder(w).Encode([]any{row})
		} else {
			json.NewEncoder(w).Encode(row)
		}
	}))
	defer server.Close()
	g := NewGitHub(func() string { return "synthetic-token" })
	g.base = server.URL
	p, yes, err := g.ReadAuditPR(context.Background(), "https://github.com/org/project", 7)
	if err != nil || !yes || p.Number != 7 {
		t.Fatal(p, yes, err)
	}
	owner = "other/project"
	if _, _, err = g.ReadAuditPR(context.Background(), "https://github.com/org/project", 7); err == nil {
		t.Fatal("wrong repository accepted")
	}
	owner = "org/project"
	number = 8
	if _, _, err = g.ReadAuditPR(context.Background(), "https://github.com/org/project", 7); err == nil {
		t.Fatal("wrong PR accepted")
	}
	number = 7
	list = true
	page, err := g.ListAuditPRs(context.Background(), "https://github.com/org/project", 1)
	if err != nil || page.More || len(page.PRs) != 1 || len(page.Anchor) != 64 {
		t.Fatal(page, err)
	}
}
