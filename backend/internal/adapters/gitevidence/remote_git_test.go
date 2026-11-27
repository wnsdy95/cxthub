package gitevidence

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const remoteGitTestOrigin = "https://github.com/org/project"

func remoteGitTestSHA(letter string) string { return strings.Repeat(letter, 40) }

// remoteGitTestRow is a commit as the provider lists it.
func remoteGitTestRow(sha, message string, parents ...string) map[string]any {
	listed := []map[string]string{}
	for _, parent := range parents {
		listed = append(listed, map[string]string{"sha": parent})
	}
	return map[string]any{
		"sha": sha,
		"commit": map[string]any{
			"author":    map[string]string{"name": "Author", "email": "author@example.com", "date": "2031-01-02T05:04:05+02:00"},
			"committer": map[string]string{"name": "Committer", "email": "committer@example.com", "date": "2031-01-02T03:10:00Z"},
			"message":   message,
		},
		"parents": listed,
		// The account the provider matched to the author is not kept.
		"author": map[string]any{"login": "someone", "id": 1},
	}
}

func remoteGitTestProvider(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *GitHub {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing configured credential")
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	provider := NewGitHub(func() string { return "test" })
	provider.base = server.URL
	return provider
}

func TestGitHubReadsRemoteCommits(t *testing.T) {
	a, b, c := remoteGitTestSHA("a"), remoteGitTestSHA("b"), remoteGitTestSHA("c")
	var rows []map[string]any
	status := http.StatusOK
	var path string
	var query map[string]string
	provider := remoteGitTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, map[string]string{}
		for key, values := range r.URL.Query() {
			query[key] = values[0]
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"No commit found for SHA"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	ctx := context.Background()

	rows = []map[string]any{
		remoteGitTestRow(a, "Join the pages\nof one list\n\nThe body is not kept.\n", b, c),
		remoteGitTestRow(b, "Start\r\n"),
	}
	commits, more, err := provider.ListRemoteCommits(ctx, remoteGitTestOrigin, a, 3)
	if err != nil || more || len(commits) != 2 {
		t.Fatalf("commits: %+v %v %v", commits, more, err)
	}
	if path != "/repos/org/project/commits" || !reflect.DeepEqual(query, map[string]string{"sha": a, "per_page": "100", "page": "3"}) {
		t.Fatalf("asked for %s %v", path, query)
	}
	want := domain.RemoteCommit{SHA: a, Parents: []string{b, c}, AuthorName: "Author", AuthorEmail: "author@example.com",
		AuthoredAt: time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC), CommittedAt: time.Date(2031, 1, 2, 3, 10, 0, 0, time.UTC), Subject: "Join the pages of one list"}
	if !reflect.DeepEqual(commits[0], want) {
		t.Fatalf("commit:\n got %+v\nwant %+v", commits[0], want)
	}
	if commits[1].Subject != "Start" || commits[1].Parents == nil || len(commits[1].Parents) != 0 || commits[1].Validate() != nil {
		t.Fatalf("a commit without parents: %+v", commits[1])
	}

	// A full page says there is another one; an empty page is a page.
	rows = rows[:0]
	for index := 0; index < 100; index++ {
		rows = append(rows, remoteGitTestRow(strings.Repeat("0", 37)+strconv.Itoa(100+index), "commit"))
	}
	if commits, more, err = provider.ListRemoteCommits(ctx, remoteGitTestOrigin, a, 1); err != nil || !more || len(commits) != 100 {
		t.Fatalf("a full page: %d %v %v", len(commits), more, err)
	}
	rows = []map[string]any{}
	if commits, more, err = provider.ListRemoteCommits(ctx, remoteGitTestOrigin, a, 2); err != nil || more || commits == nil || len(commits) != 0 {
		t.Fatalf("an empty page: %+v %v %v", commits, more, err)
	}

	// What is not a whole commit fails its page: nothing of it is returned.
	for name, change := range map[string]func(row map[string]any){
		"a short number":          func(row map[string]any) { row["sha"] = a[:12] },
		"a parent without number": func(row map[string]any) { row["parents"] = []map[string]string{{"sha": "main"}} },
		"no parent list":          func(row map[string]any) { delete(row, "parents") },
		"no author":               func(row map[string]any) { row["commit"].(map[string]any)["author"] = nil },
		"no committer":            func(row map[string]any) { delete(row["commit"].(map[string]any), "committer") },
		"no author time": func(row map[string]any) {
			row["commit"].(map[string]any)["author"] = map[string]string{"name": "Author", "email": "author@example.com"}
		},
		"a commit time that is not a time": func(row map[string]any) {
			row["commit"].(map[string]any)["committer"] = map[string]string{"date": "yesterday"}
		},
	} {
		broken := remoteGitTestRow(b, "Start")
		change(broken)
		rows = []map[string]any{remoteGitTestRow(a, "Good", b), broken}
		if commits, _, err := provider.ListRemoteCommits(ctx, remoteGitTestOrigin, a, 1); !errors.Is(err, domain.ErrIntegrity) || commits != nil {
			t.Errorf("%s: %+v %v", name, commits, err)
		}
	}
	rows = make([]map[string]any, 101)
	for index := range rows {
		rows[index] = remoteGitTestRow(a, "more than a page")
	}
	if _, _, err := provider.ListRemoteCommits(ctx, remoteGitTestOrigin, a, 1); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("more rows than a page holds: %v", err)
	}

	// A commit the provider does not have is told apart by its status.
	for _, refused := range []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusBadGateway} {
		status = refused
		var failure outbound.GitProviderStatusError
		if _, _, err := provider.ListRemoteCommits(ctx, remoteGitTestOrigin, a, 1); !errors.As(err, &failure) || failure.Status != refused || strings.Contains(err.Error(), "No commit") {
			t.Fatalf("status %d: %v", refused, err)
		}
	}
	status = http.StatusOK

	// The commits of a pull request are read the same way.
	rows = []map[string]any{remoteGitTestRow(b, "First"), remoteGitTestRow(a, "Second", b)}
	commits, more, err = provider.ListRemotePullCommits(ctx, remoteGitTestOrigin, 7, 2)
	if err != nil || more || len(commits) != 2 || commits[0].SHA != b || commits[1].SHA != a {
		t.Fatalf("pull request commits: %+v %v %v", commits, more, err)
	}
	if path != "/repos/org/project/pulls/7/commits" || !reflect.DeepEqual(query, map[string]string{"per_page": "100", "page": "2"}) {
		t.Fatalf("asked for %s %v", path, query)
	}

	// What cannot be a read is refused before the provider is asked.
	path = ""
	if _, _, err := provider.ListRemoteCommits(ctx, remoteGitTestOrigin, a, 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("page zero: %v", err)
	}
	if _, _, err := provider.ListRemoteCommits(ctx, remoteGitTestOrigin, "main", 1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a name instead of a commit number: %v", err)
	}
	if _, _, err := provider.ListRemotePullCommits(ctx, remoteGitTestOrigin, 0, 1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("pull request zero: %v", err)
	}
	if _, _, err := provider.ListRemoteCommits(ctx, "https://example.invalid/org/project", a, 1); err == nil {
		t.Fatal("an origin on another host was read")
	}
	if path != "" {
		t.Fatalf("a refused read reached the provider: %s", path)
	}
}

func TestGitHubReadsRemotePulls(t *testing.T) {
	a, b, c := remoteGitTestSHA("a"), remoteGitTestSHA("b"), remoteGitTestSHA("c")
	merged := "2031-01-02T03:04:05Z"
	pull := func(number int, state string, mergedAt *string, mergeSHA any, headRepo any) map[string]any {
		return map[string]any{
			"number": number, "title": "Pull request " + strconv.Itoa(number), "state": state, "merged_at": mergedAt, "merge_commit_sha": mergeSHA,
			"updated_at": "2031-01-02T03:04:05Z",
			"head":       map[string]any{"ref": "topic", "sha": a, "repo": headRepo},
			"base":       map[string]any{"ref": "main", "sha": b, "repo": map[string]any{"id": 91}},
		}
	}
	same, other := map[string]any{"id": 91}, map[string]any{"id": 92}
	rows := []map[string]any{}
	var path string
	var query map[string]string
	provider := remoteGitTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, map[string]string{}
		for key, values := range r.URL.Query() {
			query[key] = values[0]
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	ctx := context.Background()

	rows = []map[string]any{
		pull(4, "open", nil, c, same), // the provider's trial merge is not a merge
		pull(3, "closed", &merged, c, same),
		pull(2, "closed", nil, c, other),
		pull(1, "closed", &merged, nil, nil), // the head repository is gone
	}
	pulls, more, err := provider.ListRemotePulls(ctx, remoteGitTestOrigin, 2)
	if err != nil || more || len(pulls) != 4 {
		t.Fatalf("pull requests: %+v %v %v", pulls, more, err)
	}
	if path != "/repos/org/project/pulls" || !reflect.DeepEqual(query, map[string]string{"state": "all", "sort": "updated", "direction": "desc", "per_page": "100", "page": "2"}) {
		t.Fatalf("asked for %s %v", path, query)
	}
	updated := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	want := []domain.RemotePull{
		{Number: 4, Title: "Pull request 4", State: domain.RemotePullOpen, HeadRef: "topic", HeadSHA: a, BaseRef: "main", Commits: []string{}, UpdatedAt: updated},
		{Number: 3, Title: "Pull request 3", State: domain.RemotePullMerged, HeadRef: "topic", HeadSHA: a, BaseRef: "main", MergeSHA: c, Commits: []string{}, UpdatedAt: updated},
		{Number: 2, Title: "Pull request 2", State: domain.RemotePullClosed, HeadRef: "topic", HeadSHA: a, BaseRef: "main", Fork: true, Commits: []string{}, UpdatedAt: updated},
		{Number: 1, Title: "Pull request 1", State: domain.RemotePullMerged, HeadRef: "topic", HeadSHA: a, BaseRef: "main", Fork: true, Commits: []string{}, UpdatedAt: updated},
	}
	if !reflect.DeepEqual(pulls, want) {
		t.Fatalf("pull requests:\n got %+v\nwant %+v", pulls, want)
	}

	rows = rows[:0]
	for index := 0; index < 100; index++ {
		rows = append(rows, pull(index+1, "open", nil, nil, same))
	}
	if pulls, more, err = provider.ListRemotePulls(ctx, remoteGitTestOrigin, 1); err != nil || !more || len(pulls) != 100 {
		t.Fatalf("a full page: %d %v %v", len(pulls), more, err)
	}
	rows = []map[string]any{}
	if pulls, more, err = provider.ListRemotePulls(ctx, remoteGitTestOrigin, 1); err != nil || more || pulls == nil || len(pulls) != 0 {
		t.Fatalf("an empty page: %+v %v %v", pulls, more, err)
	}

	for name, change := range map[string]func(row map[string]any){
		"no number":                func(row map[string]any) { row["number"] = 0 },
		"an unknown state":         func(row map[string]any) { row["state"] = "draft" },
		"merged while open":        func(row map[string]any) { row["state"], row["merged_at"] = "open", merged },
		"a head without a number":  func(row map[string]any) { row["head"] = map[string]any{"ref": "topic", "sha": "", "repo": same} },
		"no head name":             func(row map[string]any) { row["head"] = map[string]any{"ref": "", "sha": a, "repo": same} },
		"no base repository":       func(row map[string]any) { row["base"] = map[string]any{"ref": "main", "sha": b, "repo": nil} },
		"no update time":           func(row map[string]any) { delete(row, "updated_at") },
		"a merge without a number": func(row map[string]any) { row["merged_at"], row["merge_commit_sha"] = merged, "merged" },
	} {
		broken := pull(9, "closed", nil, nil, same)
		change(broken)
		rows = []map[string]any{pull(8, "open", nil, nil, same), broken}
		if pulls, _, err := provider.ListRemotePulls(ctx, remoteGitTestOrigin, 1); !errors.Is(err, domain.ErrIntegrity) || pulls != nil {
			t.Errorf("%s: %+v %v", name, pulls, err)
		}
	}
	if _, _, err := provider.ListRemotePulls(ctx, remoteGitTestOrigin, 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("page zero: %v", err)
	}
}

func TestGitHubReadsRemoteDefaultBranch(t *testing.T) {
	answer := map[string]any{"default_branch": "trunk", "full_name": "org/project"}
	var path string
	provider := remoteGitTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewEncoder(w).Encode(answer)
	})
	ctx := context.Background()
	branch, err := provider.ReadRemoteDefaultBranch(ctx, "git@github.com:org/project.git")
	if err != nil || branch != "trunk" || path != "/repos/org/project" {
		t.Fatalf("default branch %q from %s: %v", branch, path, err)
	}
	answer = map[string]any{"full_name": "org/project"}
	if _, err := provider.ReadRemoteDefaultBranch(ctx, remoteGitTestOrigin); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("a repository without a default branch: %v", err)
	}
	if _, err := provider.ReadRemoteDefaultBranch(ctx, "https://example.invalid/org/project"); err == nil {
		t.Fatal("an origin on another host was read")
	}
}

// A repository's own credentials are resolved for reads below the repository
// and may name it by its current name. The repository itself is read with the
// credential of that scope, and from the name the resolver gave.
func TestGitHubReadsRemoteDefaultBranchWithRepositoryCredentials(t *testing.T) {
	var path, authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, authorization = r.URL.Path, r.Header.Get("Authorization")
		if strings.HasSuffix(path, "/pulls") {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
	}))
	defer server.Close()
	var asked []string
	provider := NewAuthenticated(func(_ context.Context, requested string) (string, string, string, error) {
		asked = append(asked, requested)
		const scope = "/repos/org/project/"
		if !strings.HasPrefix(requested, scope) {
			return "", "", "", domain.ErrForbidden
		}
		return "scoped", "installation:1", "/repos/org/renamed/" + requested[len(scope):], nil
	})
	provider.base = server.URL
	branch, err := provider.ReadRemoteDefaultBranch(context.Background(), remoteGitTestOrigin)
	if err != nil || branch != "main" {
		t.Fatalf("default branch %q: %v", branch, err)
	}
	if !reflect.DeepEqual(asked, []string{"/repos/org/project/"}) || path != "/repos/org/renamed" || authorization != "Bearer scoped" {
		t.Fatalf("resolved %v, asked the provider for %s as %q", asked, path, authorization)
	}
	// The other reads stay below the repository as they are.
	if _, _, err := provider.ListRemotePulls(context.Background(), remoteGitTestOrigin, 1); err != nil || path != "/repos/org/renamed/pulls" {
		t.Fatalf("pull requests from %s: %v", path, err)
	}
}
