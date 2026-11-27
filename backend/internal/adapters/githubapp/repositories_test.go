package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// repositoriesClient is a client whose GitHub answers the installation token
// request itself and leaves the repository list to answer.
func repositoriesClient(t *testing.T, tokens, lists *atomic.Int32, answer func(w http.ResponseWriter, r *http.Request, page string)) *Client {
	t.Helper()
	return testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/access_tokens") {
			fmt.Fprintf(w, `{"token":"token-%d","expires_at":%q}`, tokens.Add(1), time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		if r.Method != "GET" || r.URL.Path != "/installation/repositories" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer token-") {
			t.Errorf("the list was asked without the installation's token: %q", got)
		}
		lists.Add(1)
		answer(w, r, r.URL.Query().Get("page"))
	})
}

func repositoryJSON(id int, name string, private bool, owner int) string {
	return fmt.Sprintf(`{"id":%d,"full_name":%q,"private":%t,"owner":{"id":%d,"login":"team","type":"Organization"}}`, id, name, private, owner)
}

func TestRepositoriesListsAnInstallationPageByPage(t *testing.T) {
	var tokens, lists atomic.Int32
	client := repositoriesClient(t, &tokens, &lists, func(w http.ResponseWriter, _ *http.Request, page string) {
		var items []string
		switch page {
		case "1":
			for id := 1; id <= 100; id++ {
				items = append(items, repositoryJSON(id, fmt.Sprintf("team/app-%d", id), id%2 == 0, 7))
			}
		case "2":
			for id := 101; id <= 103; id++ {
				items = append(items, repositoryJSON(id, fmt.Sprintf("team/app-%d", id), true, 7))
			}
		default:
			t.Errorf("a page after the short one was asked for: %s", page)
		}
		fmt.Fprintf(w, `{"repositories":[%s]}`, strings.Join(items, ","))
	})
	got, err := client.Repositories(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 103 || lists.Load() != 2 || tokens.Load() != 1 {
		t.Fatalf("repositories=%d list requests=%d token requests=%d", len(got), lists.Load(), tokens.Load())
	}
	for index, repository := range got {
		want := domain.GitHubRepository{ID: int64(index + 1), AccountID: 7, FullName: fmt.Sprintf("team/app-%d", index+1), Private: index+1 > 100 || (index+1)%2 == 0}
		if repository != want {
			t.Fatalf("repository %d: %+v, want %+v", index, repository, want)
		}
	}
}

func TestRepositoriesOfAnEmptyInstallationIsAnEmptyList(t *testing.T) {
	var tokens, lists atomic.Int32
	client := repositoriesClient(t, &tokens, &lists, func(w http.ResponseWriter, _ *http.Request, _ string) {
		fmt.Fprint(w, `{"repositories":[]}`)
	})
	got, err := client.Repositories(context.Background(), 9)
	if err != nil || got == nil || len(got) != 0 || lists.Load() != 1 {
		t.Fatalf("an installation without repositories: %v %v requests=%d", got, err, lists.Load())
	}
}

func TestRepositoriesRefusesAnAnswerItCannotTrust(t *testing.T) {
	for name, body := range map[string]string{
		"no list at all":             `{}`,
		"a null list":                `{"repositories":null}`,
		"a repository without id":    `{"repositories":[` + repositoryJSON(0, "team/app", false, 7) + `]}`,
		"a repository without owner": `{"repositories":[` + repositoryJSON(1, "team/app", false, 0) + `]}`,
		"a name of one part":         `{"repositories":[` + repositoryJSON(1, "app", false, 7) + `]}`,
		"a name of three parts":      `{"repositories":[` + repositoryJSON(1, "team/app/extra", false, 7) + `]}`,
		"a name that climbs":         `{"repositories":[` + repositoryJSON(1, "../app", false, 7) + `]}`,
		"a name with a space":        `{"repositories":[` + repositoryJSON(1, "team/my app", false, 7) + `]}`,
		"not JSON":                   `<html>`,
		"one good, one bad":          `{"repositories":[` + repositoryJSON(1, "team/app", false, 7) + `,` + repositoryJSON(2, "team/.", false, 7) + `]}`,
	} {
		var tokens, lists atomic.Int32
		client := repositoriesClient(t, &tokens, &lists, func(w http.ResponseWriter, _ *http.Request, _ string) { fmt.Fprint(w, body) })
		if got, err := client.Repositories(context.Background(), 9); !errors.Is(err, domain.ErrIntegrity) || got != nil {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
}

func TestRepositoriesForgetsATokenGitHubRefused(t *testing.T) {
	var tokens, lists atomic.Int32
	refuse := true
	client := repositoriesClient(t, &tokens, &lists, func(w http.ResponseWriter, _ *http.Request, _ string) {
		if refuse {
			http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{"repositories":[`+repositoryJSON(1, "team/app", false, 7)+`]}`)
	})
	if _, err := client.Repositories(context.Background(), 9); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a refused list: %v", err)
	}
	// The token that was refused is not used again: the next read asks for a new one.
	refuse = false
	if got, err := client.Repositories(context.Background(), 9); err != nil || len(got) != 1 || tokens.Load() != 2 {
		t.Fatalf("after the refusal: %v %v token requests=%d", got, err, tokens.Load())
	}
	// A list that is simply not there keeps the token.
	missing := repositoriesClient(t, &tokens, &lists, func(w http.ResponseWriter, _ *http.Request, _ string) { http.NotFound(w, nil) })
	before := tokens.Load()
	if _, err := missing.Repositories(context.Background(), 9); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a missing list: %v", err)
	}
	if _, err := missing.Repositories(context.Background(), 9); !errors.Is(err, domain.ErrNotFound) || tokens.Load() != before+1 {
		t.Fatalf("a missing list asked for a token again: token requests %d -> %d", before, tokens.Load())
	}
}

func TestRepositoriesStopsAtTheLargestListItReconciles(t *testing.T) {
	var tokens, lists atomic.Int32
	client := repositoriesClient(t, &tokens, &lists, func(w http.ResponseWriter, _ *http.Request, page string) {
		var items []string
		for id := 1; id <= 100; id++ {
			items = append(items, repositoryJSON(id, "team/app", false, 7))
		}
		fmt.Fprintf(w, `{"repositories":[%s]}`, strings.Join(items, ","))
	})
	if got, err := client.Repositories(context.Background(), 9); err == nil || got != nil || lists.Load() != 100 {
		t.Fatalf("a list that never ends: repositories=%d err=%v requests=%d", len(got), err, lists.Load())
	}
}

func TestValidRepositoryName(t *testing.T) {
	for name, want := range map[string]bool{
		"team/app": true, "Team_1/app.name-2": true, "a/b": true,
		"": false, "app": false, "team/": false, "/app": false, "team/app/more": false,
		"./app": false, "team/..": false, "team/my app": false, "team/app?x": false, "te am/app": false, "team/\uc571": false,
	} {
		if got := validRepositoryName(name); got != want {
			t.Errorf("%q: %t", name, got)
		}
	}
}

func TestGraphQLRateLimitIsReadFromTheAnswer(t *testing.T) {
	limited := `{"errors":[{"type":"RATE_LIMITED","message":"slow down"}]}`
	other := `{"errors":[{"type":"NOT_FOUND"}]}`
	for name, scenario := range map[string]struct {
		body    string
		headers map[string]string
		want    bool
	}{
		"the error says so":                   {limited, nil, true},
		"an error and nothing left":           {other, map[string]string{"X-RateLimit-Remaining": "0"}, true},
		"an error and a time to come back":    {other, map[string]string{"Retry-After": "30"}, true},
		"another error":                       {other, nil, false},
		"no error with nothing left":          {`{"data":{}}`, map[string]string{"X-RateLimit-Remaining": "0"}, false},
		"an empty list of errors":             {`{"errors":[]}`, map[string]string{"Retry-After": "30"}, false},
		"an answer that is not JSON":          {`<html>`, map[string]string{"Retry-After": "30"}, false},
		"several errors, one of them a limit": {`{"errors":[{"type":"NOT_FOUND"},{"type":"RATE_LIMITED"}]}`, nil, true},
	} {
		headers := http.Header{}
		for key, value := range scenario.headers {
			headers.Set(key, value)
		}
		if got := graphQLRateLimited([]byte(scenario.body), headers); got != scenario.want {
			t.Errorf("%s: %t", name, got)
		}
	}
}

func TestGraphQLRateLimitDefersTheNextRequest(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/graphql" {
			fmt.Fprint(w, `{"errors":[{"type":"RATE_LIMITED"}]}`)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	var out struct{}
	if err := client.request(context.Background(), "POST", client.api+"/graphql", "", "installation:1", map[string]any{}, &out); err == nil {
		t.Fatal("a rate limited answer was taken as an answer")
	}
	// The same installation is not asked again; another one is.
	if err := client.request(context.Background(), "POST", client.api+"/graphql", "", "installation:1", map[string]any{}, &out); err == nil || calls.Load() != 1 {
		t.Fatalf("the limited installation was asked again: calls=%d err=%v", calls.Load(), err)
	}
	if err := client.request(context.Background(), "POST", client.api+"/graphql", "", "installation:2", map[string]any{}, &out); err == nil || calls.Load() != 2 {
		t.Fatalf("another installation was not asked: calls=%d err=%v", calls.Load(), err)
	}
}
