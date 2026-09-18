package gitevidence

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGitHubReadsExactTreesAndRejectsIncompleteEvidence(t *testing.T) {
	oid := func(c string) string { return strings.Repeat(c, 40) }
	parent, commit, oldTree, newTree := oid("1"), oid("2"), oid("3"), oid("4")
	truncated, omitTree, badCommit, redirect := false, false, false, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing configured credential")
		}
		switch r.URL.Path {
		case "/repos/org/project/git/commits/" + commit:
			if redirect {
				http.Redirect(w, r, "http://127.0.0.1:1/credentials", 302)
				return
			}
			sha := commit
			if badCommit {
				sha = parent
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "tree": map[string]string{"sha": newTree}, "parents": []map[string]string{{"sha": parent}}})
		case "/repos/org/project/git/commits/" + parent:
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": parent, "tree": map[string]string{"sha": oldTree}, "parents": []any{}})
		case "/repos/org/project/git/trees/" + oldTree:
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": oldTree, "truncated": false, "tree": []map[string]string{{"path": "untouched", "type": "blob", "mode": "100644", "sha": oid("5")}, {"path": "feature", "type": "blob", "mode": "100644", "sha": oid("6")}}})
		case "/repos/org/project/git/trees/" + newTree:
			if r.URL.Query().Get("recursive") != "1" {
				t.Error("nonrecursive tree masquerades as complete")
			}
			out := map[string]any{"sha": newTree, "truncated": truncated}
			if !omitTree {
				out["tree"] = []map[string]string{{"path": "untouched", "type": "blob", "mode": "100644", "sha": oid("5")}, {"path": "feature", "type": "blob", "mode": "100755", "sha": oid("7")}}
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/repos/org/project/compare/" + parent + "..." + commit:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "base_commit": map[string]string{"sha": parent}, "merge_base_commit": map[string]string{"sha": parent}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	g := NewGitHub(func() string { return "test" })
	g.base = server.URL
	got, err := g.ReadCommitDelta(context.Background(), "git@github.com:org/project.git", commit, "")
	if err != nil || !got.Complete || got.Parent != parent || len(got.Changes) != 1 || got.Changes[0].Path != "feature" || got.Changes[0].After.Mode != "100755" {
		t.Fatalf("delta=%+v error=%v", got, err)
	}
	if yes, err := g.IsGitAncestor(context.Background(), "https://github.com/org/project", parent, commit); err != nil || !yes {
		t.Fatal("ancestry", yes, err)
	}
	truncated = true
	got, err = g.ReadCommitDelta(context.Background(), "git@github.com:org/project.git", commit, "")
	if err != nil || got.Complete || len(got.Changes) > 0 {
		t.Fatalf("truncated tree accepted: %+v %v", got, err)
	}
	truncated = false
	omitTree = true
	if _, err = g.ReadCommitDelta(context.Background(), "git@github.com:org/project.git", commit, ""); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("missing tree interpreted as empty", err)
	}
	omitTree = false
	badCommit = true
	if _, err = g.ReadCommitDelta(context.Background(), "git@github.com:org/project.git", commit, ""); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("wrong commit accepted", err)
	}
	badCommit = false
	redirect = true
	if _, err = g.ReadCommitDelta(context.Background(), "git@github.com:org/project.git", commit, ""); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatal("redirect followed", err)
	}
	for _, origin := range []string{"https://attacker.invalid/org/repo", "https://github.com/org/../repo", "https://github.com/org/project?token=bad"} {
		if _, err := g.ReadCommitDelta(context.Background(), origin, commit, ""); err == nil {
			t.Fatal("untrusted origin accepted", origin)
		}
	}
}
