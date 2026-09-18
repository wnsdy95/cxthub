package gitevidence

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGitHubReadsExactTreesAndRejectsIncompleteEvidence(t *testing.T) {
	oid := func(c string) string { return strings.Repeat(c, 40) }
	parent, commit, oldTree, newTree := oid("1"), oid("2"), oid("3"), oid("4")
	oldNode, _ := domain.NewGitTreeNode(map[string]domain.GitEntry{"untouched": {OID: oid("5"), Mode: "100644"}, "feature": {OID: oid("6"), Mode: "100644"}}, 40)
	newNode, _ := domain.NewGitTreeNode(map[string]domain.GitEntry{"untouched": {OID: oid("5"), Mode: "100644"}, "feature": {OID: oid("7"), Mode: "100755"}}, 40)
	oldTree, newTree = oldNode.OID, newNode.OID
	corruptParent := false
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
			if corruptParent {
				json.NewEncoder(w).Encode(map[string]any{"sha": oldTree, "truncated": false, "tree": []any{}})
				return
			}
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
	all, err := g.ReadCommitDeltas(context.Background(), "git@github.com:org/project.git", commit)
	if err != nil || len(all) != 1 || !all[0].Complete || all[0].Parent != parent {
		t.Fatal("automatic comparison", all, err)
	}
	corruptParent = true
	if _, e := g.ReadCommitDeltas(context.Background(), "git@github.com:org/project.git", commit); !errors.Is(e, domain.ErrIntegrity) {
		t.Fatal("corrupt comparison parent accepted", e)
	}
	corruptParent = false
	truncated = true
	if _, e := g.ReadCommitDeltas(context.Background(), "git@github.com:org/project.git", commit); !errors.Is(e, domain.ErrIntegrity) {
		t.Fatal("automatic truncated index", e)
	}
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
	g := NewGitHub(nil)
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

func TestGitHubCommitTreeRehashesCompleteProviderResponse(t *testing.T) {
	commit := strings.Repeat("c", 40)
	entry := domain.GitEntry{OID: strings.Repeat("a", 40), Mode: "100644"}
	node, _ := domain.NewGitTreeNode(map[string]domain.GitEntry{"file": entry}, 40)
	omit, truncated := false, false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git/commits/") {
			json.NewEncoder(w).Encode(map[string]any{"sha": commit, "tree": map[string]string{"sha": node.OID}, "parents": []any{}})
			return
		}
		entries := []map[string]string{}
		if !omit {
			entries = append(entries, map[string]string{"path": "file", "sha": entry.OID, "type": "blob", "mode": entry.Mode})
		}
		json.NewEncoder(w).Encode(map[string]any{"sha": node.OID, "tree": entries, "truncated": truncated})
	}))
	defer ts.Close()
	g := NewGitHub(nil)
	g.base = ts.URL
	got, e := g.ReadCommitTree(context.Background(), "https://github.com/org/project", commit)
	if e != nil || got.Commit.Tree != node.OID {
		t.Fatal(got, e)
	}
	// A provider may mark a clipped response complete. Hash verification still rejects it.
	omit = true
	if _, e = g.ReadCommitTree(context.Background(), "https://github.com/org/project", commit); !errors.Is(e, domain.ErrIntegrity) {
		t.Fatal("omission accepted", e)
	}
	omit = false
	truncated = true
	if _, e = g.ReadCommitTree(context.Background(), "https://github.com/org/project", commit); !errors.Is(e, domain.ErrIntegrity) {
		t.Fatal("partial tree accepted", e)
	}
}
