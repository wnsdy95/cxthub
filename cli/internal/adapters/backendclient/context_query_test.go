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

func TestHistoryContextQueryPreservesServerReceiptAndFailsClosed(t *testing.T) {
	for _, mode := range []string{"success", "unknown version", "wrong repo", "wrong position", "wrong code", "duplicate", "order mismatch", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			repo := string(domain.HashContent([]byte("repo")))
			id := domain.HashContent([]byte("source"))
			state := domain.HashContent([]byte("receipt"))
			code := strings.Repeat("a", 40)
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				q := r.URL.Query()
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer token" || q.Get("position") != string(id) || q.Get("code_commit") != code || q.Get("branch") != "feature/a+b" || q.Get("scope") != "current" {
					t.Error("bad request", r.Method, r.URL)
				}
				if mode == "revoked" {
					http.Error(w, "denied", http.StatusForbidden)
					return
				}
				view := domain.ContextQueryView{Version: 1, Branch: "feature/a+b", StateHash: state, Position: id, Revision: domain.RepositoryRevision{Evidence: 9007199254740993, Graph: 42}, Snapshots: []domain.Snapshot{{ID: id, RepoID: repo}}, Inclusion: &domain.BranchContext{CodeCommit: code, SnapshotIDs: []domain.ContentHash{id}}}
				switch mode {
				case "unknown version":
					view.Version = 2
				case "wrong repo":
					view.Snapshots[0].RepoID = "other"
				case "wrong position":
					view.Position = state
				case "wrong code":
					view.Inclusion.CodeCommit = strings.Repeat("b", 40)
				case "duplicate":
					view.Snapshots = append(view.Snapshots, view.Snapshots[0])
				case "order mismatch":
					view.Inclusion.SnapshotIDs = []domain.ContentHash{state}
				}
				_ = json.NewEncoder(w).Encode(view)
			}))
			defer ts.Close()
			c := NewBackendClient(func() string { return ts.URL }, func() string { return "token" }, domain.TeamIdentity{})
			out, err := c.QueryContext(context.Background(), repo, domain.ContextSelection{Position: string(id), CodeCommit: code, Scope: "current", Branch: "feature/a+b"})
			if mode == "success" {
				if err != nil || out.StateHash != state || out.Revision.Evidence != 9007199254740993 {
					t.Fatal(out, err)
				}
			} else if err == nil {
				t.Fatal("unsafe response accepted", out)
			}
			if calls != 1 {
				t.Fatal("unexpected registration or fallback", calls)
			}
		})
	}
}
