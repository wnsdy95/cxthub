package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type repositoryListPage struct {
	Notice       string `json:"notice"`
	Repositories []struct {
		ID            string `json:"id"`
		Path          string `json:"path"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repositories"`
	Next string `json:"next_cursor"`
}

func repositoryListCall(t *testing.T, server *Server, user domain.User, args map[string]any) (repositoryListPage, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	text, err := server.runTool(systemTestContext(), user, "repository_list", raw)
	if err != nil {
		return repositoryListPage{}, err
	}
	var page repositoryListPage
	if err := json.Unmarshal([]byte(text), &page); err != nil {
		t.Fatalf("the answer is not a page: %v: %s", err, text)
	}
	return page, nil
}

// The list of repositories a caller may read comes in pages: fifty at a time
// unless the caller asks otherwise, never more than a hundred, in the order
// of their paths, each repository once. A query narrows it to the paths that
// hold it.
func TestRepositoryListComesInBoundedPages(t *testing.T) {
	ctx := systemTestContext()
	storage := store.NewFSStore(t.TempDir())
	user := domain.User{ID: "lister", Email: "lister@example.test", Name: "Lister", Username: "lister"}
	identity := app.NewIdentityService(fixedVerifier{user: user}, storage)
	if _, _, err := identity.Login(ctx, "idp-token", "test"); err != nil {
		t.Fatal(err)
	}
	const total = 105
	var repos []domain.Repo
	for index := 0; index < total; index++ {
		repository, err := identity.CreateRepository(ctx, user, fmt.Sprintf("listed-%03d", index))
		if err != nil {
			t.Fatal(err)
		}
		repo := domain.Repo{ID: pageHash(index + 1), RepositoryID: repository.ID}
		if index == 7 {
			repo.DefaultBranch = "trunk"
		}
		repos = append(repos, repo)
	}
	// A repository the caller may not read is not in the list.
	stranger := domain.User{ID: "stranger", Email: "stranger@example.test", Name: "Stranger", Username: "stranger"}
	if err := storage.UpsertUser(ctx, stranger); err != nil {
		t.Fatal(err)
	}
	hidden, err := identity.CreateRepository(ctx, stranger, "hidden")
	if err != nil {
		t.Fatal(err)
	}
	repos = append(repos, domain.Repo{ID: pageHash(total + 1), RepositoryID: hidden.ID})
	// The store hands them over in no useful order: here, the last first.
	for left, right := 0, len(repos)-1; left < right; left, right = left+1, right-1 {
		repos[left], repos[right] = repos[right], repos[left]
	}
	server, err := NewServer(fakeContextBackend{repos: repos}, identity, storage, "https://cxthub.test")
	if err != nil {
		t.Fatal(err)
	}

	// Without a limit: fifty, then fifty, then the last five.
	var paths []string
	seen := map[string]bool{}
	args := map[string]any{}
	for _, want := range []int{50, 50, 5} {
		page, err := repositoryListCall(t, server, user, args)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Repositories) != want || (page.Next == "") != (want == 5) || page.Notice == "" {
			t.Fatalf("a page of %d repositories, next cursor %q; want %d", len(page.Repositories), page.Next, want)
		}
		for _, listed := range page.Repositories {
			if seen[listed.ID] || !strings.HasPrefix(listed.Path, "lister/listed-") || listed.DefaultBranch == "" {
				t.Fatalf("listed twice, or not the caller's: %+v", listed)
			}
			seen[listed.ID] = true
			paths = append(paths, listed.Path)
		}
		args["cursor"] = page.Next
	}
	if len(paths) != total {
		t.Fatalf("%d repositories listed, want %d", len(paths), total)
	}
	for index := 1; index < len(paths); index++ {
		if paths[index-1] >= paths[index] {
			t.Fatalf("not in the order of their paths: %s before %s", paths[index-1], paths[index])
		}
	}

	// The caller's limit is taken, up to a hundred.
	for limit, want := range map[int]int{1: 1, 20: 20, 100: 100, 101: 100, 5000: 100, -3: 50} {
		page, err := repositoryListCall(t, server, user, map[string]any{"limit": limit})
		if err != nil || len(page.Repositories) != want || page.Next == "" {
			t.Errorf("limit %d: %d repositories, next cursor %q, %v; want %d", limit, len(page.Repositories), page.Next, err, want)
		}
	}

	// A query narrows the list to the paths that hold it, whatever its case.
	for query, want := range map[string]int{"listed-104": 1, "LISTED-10": 5, "listed-00": 10, "  listed-007  ": 1, "no-such": 0, "hidden": 0} {
		page, err := repositoryListCall(t, server, user, map[string]any{"query": query, "limit": 100})
		if err != nil || len(page.Repositories) != want || page.Next != "" {
			t.Errorf("query %q: %d repositories, next cursor %q, %v; want %d", query, len(page.Repositories), page.Next, err, want)
		}
		for _, listed := range page.Repositories {
			if !strings.Contains(listed.Path, strings.ToLower(strings.TrimSpace(query))) {
				t.Errorf("query %q listed %s", query, listed.Path)
			}
		}
	}
	if _, err := repositoryListCall(t, server, user, map[string]any{"query": strings.Repeat("q", 128)}); err != nil {
		t.Errorf("a query of 128 characters: %v", err)
	}
	if _, err := repositoryListCall(t, server, user, map[string]any{"query": strings.Repeat("q", 129)}); err == nil {
		t.Error("a query of 129 characters was accepted")
	}
	// The branch a repository names is listed; one that names none is on main.
	page, err := repositoryListCall(t, server, user, map[string]any{"query": "listed-007"})
	if err != nil || len(page.Repositories) != 1 || page.Repositories[0].DefaultBranch != "trunk" {
		t.Errorf("the default branch of a repository that names one: %+v %v", page.Repositories, err)
	}
	page, err = repositoryListCall(t, server, user, map[string]any{"query": "listed-008"})
	if err != nil || len(page.Repositories) != 1 || page.Repositories[0].DefaultBranch != "main" {
		t.Errorf("the default branch of a repository that names none: %+v %v", page.Repositories, err)
	}

	// A cursor continues the list it came from and no other.
	first, err := repositoryListCall(t, server, user, map[string]any{"limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	second, err := repositoryListCall(t, server, user, map[string]any{"limit": 10, "cursor": first.Next})
	if err != nil || len(second.Repositories) != 10 || second.Repositories[0].Path != paths[10] {
		t.Fatalf("the second page: %+v %v", second.Repositories, err)
	}
	if _, err := repositoryListCall(t, server, user, map[string]any{"limit": 10, "cursor": first.Next, "query": "listed-1"}); err == nil {
		t.Error("the cursor of one list continued another query")
	}
	if _, err := repositoryListCall(t, server, stranger, map[string]any{"limit": 10, "cursor": first.Next}); err == nil {
		t.Error("the cursor of one caller continued another caller's list")
	}
	// A cursor of this list in every respect, except that it is longer than a cursor can be.
	decoded, err := base64.RawURLEncoding.DecodeString(first.Next)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(decoded, &fields); err != nil {
		t.Fatal(err)
	}
	fields["after"] = strings.Repeat("a", 4096)
	long, _ := json.Marshal(fields)
	for name, cursor := range map[string]string{
		"letters that decode to no cursor": "not-a-cursor",
		"not base64 at all":                "%%%",
		"longer than a cursor can be":      base64.RawURLEncoding.EncodeToString(long),
	} {
		if _, err := repositoryListCall(t, server, user, map[string]any{"cursor": cursor}); err == nil {
			t.Errorf("a cursor that is not one was accepted: %s", name)
		}
	}
	// The other caller sees only their own repository.
	theirs, err := repositoryListCall(t, server, stranger, map[string]any{})
	if err != nil || len(theirs.Repositories) != 1 || theirs.Repositories[0].Path != "stranger/hidden" {
		t.Errorf("the other caller's list: %+v %v", theirs.Repositories, err)
	}
}
