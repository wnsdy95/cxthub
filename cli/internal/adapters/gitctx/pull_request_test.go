package gitctx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGitHubRepository(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		remote     string
		owner      string
		repo       string
		wantParsed bool
	}{
		{name: "scp ssh", remote: "git@github.com:Acme/Project.git", owner: "acme", repo: "project", wantParsed: true},
		{name: "https", remote: "https://github.com/Acme/Project.git", owner: "acme", repo: "project", wantParsed: true},
		{name: "ssh url", remote: "ssh://git@github.com/Acme/Project.git", owner: "acme", repo: "project", wantParsed: true},
		{name: "non github", remote: "git@gitlab.com:acme/project.git"},
		{name: "nested path", remote: "https://github.com/acme/group/project.git"},
		{name: "userinfo confusion", remote: "https://github.com@evil.example/acme/project.git"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, ok := githubRepository(tt.remote)
			if ok != tt.wantParsed || owner != tt.owner || repo != tt.repo {
				t.Fatalf("githubRepository(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.remote, owner, repo, ok, tt.owner, tt.repo, tt.wantParsed)
			}
		})
	}
}

func TestGitHubPRMergeResolver(t *testing.T) {
	t.Parallel()

	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	var requests atomic.Int32
	var authHeader atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		authHeader.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/repos/acme/project/pulls" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		for key, want := range map[string]string{
			"state": "closed", "base": "main", "sort": "updated",
			"direction": "desc", "per_page": "100",
		} {
			if got := r.URL.Query().Get(key); got != want {
				http.Error(w, "bad query "+key, http.StatusBadRequest)
				return
			}
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 1 {
			pulls := make([]map[string]any, 100)
			for i := range pulls {
				pulls[i] = mergedPullJSON(1000+i, "main", "noise-"+strconv.Itoa(i), strings.Repeat("c", 39)+strconv.Itoa(i%10), "acme/project")
			}
			_ = json.NewEncoder(w).Encode(pulls)
			return
		}
		if page != 2 {
			http.Error(w, "unexpected page", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			mergedPullJSON(22, "main", "feature/new", newSHA, "acme/project"),
			mergedPullJSON(21, "main", "feature/old", oldSHA, "acme/project"),
			mergedPullJSON(23, "other", "wrong-base", oldSHA, "acme/project"),
			mergedPullJSON(24, "main", "fork-branch", oldSHA, "fork/project"),
			{
				"number": 25, "merged_at": nil, "merge_commit_sha": oldSHA,
				"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "acme/project"}},
				"head": map[string]any{"ref": "open", "repo": map[string]any{"full_name": "acme/project"}},
			},
		})
	}))
	defer server.Close()

	resolver := newGitHubPRMergeResolver(server.Client(), server.URL, func() string { return "test-token" })
	got, err := resolver.ResolveMergedPullRequests(
		context.Background(),
		"git@github.com:Acme/Project.git",
		"main",
		[]string{oldSHA, newSHA},
	)
	if err != nil {
		t.Fatalf("ResolveMergedPullRequests: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
	if authHeader.Load() != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want bearer token", authHeader.Load())
	}
	if len(got) != 2 {
		t.Fatalf("resolved = %#v, want 2 PRs", got)
	}
	if got[0].Number != 21 || got[0].HeadBranch != "feature/old" || got[0].MergeCommitSHA != oldSHA {
		t.Fatalf("first resolved PR = %#v, want oldest incoming PR #21", got[0])
	}
	if got[1].Number != 22 || got[1].HeadBranch != "feature/new" || got[1].MergeCommitSHA != newSHA {
		t.Fatalf("second resolved PR = %#v, want newest incoming PR #22", got[1])
	}
}

func TestGitHubPRMergeResolverUnsupportedRemoteDoesNotCallAPI(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	resolver := newGitHubPRMergeResolver(server.Client(), server.URL, nil)
	got, err := resolver.ResolveMergedPullRequests(
		context.Background(),
		"git@gitlab.com:acme/project.git",
		"main",
		[]string{strings.Repeat("a", 40)},
	)
	if err != nil || len(got) != 0 {
		t.Fatalf("unsupported remote result = (%#v, %v), want empty success", got, err)
	}
	if requests.Load() != 0 {
		t.Fatalf("API requests = %d, want 0", requests.Load())
	}
}

func TestGitHubPRMergeResolverHTTPFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited with secret details", http.StatusForbidden)
	}))
	defer server.Close()

	resolver := newGitHubPRMergeResolver(server.Client(), server.URL, nil)
	_, err := resolver.ResolveMergedPullRequests(
		context.Background(),
		"https://github.com/acme/project",
		"main",
		[]string{strings.Repeat("a", 40)},
	)
	if err == nil || !strings.Contains(err.Error(), "403 Forbidden") {
		t.Fatalf("error = %v, want sanitized HTTP status", err)
	}
	if strings.Contains(err.Error(), "secret details") {
		t.Fatalf("error leaked response body: %v", err)
	}
}

func mergedPullJSON(number int, base, head, sha, headRepo string) map[string]any {
	return map[string]any{
		"number":           number,
		"merged_at":        "2026-07-30T00:00:00Z",
		"merge_commit_sha": sha,
		"base": map[string]any{
			"ref":  base,
			"repo": map[string]any{"full_name": "acme/project"},
		},
		"head": map[string]any{
			"ref":  head,
			"repo": map[string]any{"full_name": headRepo},
		},
	}
}
