package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func prHistoryResponse() map[string]any {
	return map[string]any{"data": map[string]any{"repository": map[string]any{
		"databaseId": 10,
		"pullRequest": map[string]any{
			"id": "PR_stable", "fullDatabaseId": "3500000000", "number": 488, "baseRefName": "main", "headRefName": "feature",
			"headRefOid": strings.Repeat("c", 40), "headRepository": map[string]any{"databaseId": 10},
			"merged": true, "mergedAt": "2026-10-10T10:00:00Z", "mergeCommit": map[string]any{"oid": strings.Repeat("d", 40)},
			"timelineItems": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": "cursor-1"},
				"nodes":    []any{map[string]any{"__typename": "HeadRefForcePushedEvent", "id": "rewrite-1", "createdAt": "2026-10-10T09:00:00Z", "beforeCommit": map[string]any{"oid": strings.Repeat("a", 40)}, "afterCommit": map[string]any{"oid": strings.Repeat("c", 40)}}},
			},
		},
	}}}
}

func TestPRHeadHistoryReadsScopedPage(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer installation-test" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var input struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input.Variables["owner"] != "acme" || input.Variables["name"] != "project" || input.Variables["number"] != float64(488) || input.Variables["after"] != "cursor-0" || !strings.Contains(input.Query, "HEAD_REF_FORCE_PUSHED_EVENT") {
			t.Errorf("wrong query scope: %+v", input.Variables)
		}
		json.NewEncoder(w).Encode(prHistoryResponse())
	})
	slot := client.slot(9)
	slot.token, slot.expires = "installation-test", time.Now().Add(time.Hour)
	page, err := client.PullRequestHeadHistory(context.Background(), 9, domain.GitHubRepository{ID: 10, FullName: "acme/project"}, 488, "cursor-0")
	if err != nil || page.PullRequestID != "PR_stable" || page.PullRequestDatabaseID != 3500000000 || page.HeadSHA != strings.Repeat("c", 40) || page.MergeSHA != strings.Repeat("d", 40) || len(page.Rewrites) != 1 || page.Rewrites[0].Before != strings.Repeat("a", 40) || page.EndCursor != "cursor-1" || page.HasNextPage {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestPRHeadHistoryKeepsPaginationPastIrrelevantNullableEvents(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(strconv.FormatBool(mixed), func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, request *http.Request) {
				body := prHistoryResponse()
				pr := body["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"].(map[string]any)
				timeline := pr["timelineItems"].(map[string]any)
				nodes := []any{}
				if mixed {
					nodes = timeline["nodes"].([]any)
				}
				timeline["nodes"] = append(nodes, map[string]any{"__typename": "HeadRefForcePushedEvent", "id": "later-reuse", "createdAt": "2026-10-10T11:00:00Z", "beforeCommit": nil, "afterCommit": nil})
				timeline["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "later-page"}
				json.NewEncoder(w).Encode(body)
			})
			slot := client.slot(9)
			slot.token, slot.expires = "installation-test", time.Now().Add(time.Hour)
			page, err := client.PullRequestHeadHistory(context.Background(), 9, domain.GitHubRepository{ID: 10, FullName: "acme/project"}, 488, "")
			expected := 1
			if mixed {
				expected = 2
			}
			if err != nil || !page.HasNextPage || page.EndCursor != "later-page" || len(page.Rewrites) != expected {
				t.Fatalf("irrelevant event blocked progress: %+v, %v", page, err)
			}
		})
	}
}

func TestPRHeadHistoryRejectsPartialAndMismatchedEvidence(t *testing.T) {
	for _, scenario := range []string{"graphql-error", "repository", "pr-number", "missing-before", "missing-head-repo", "missing-nodes", "missing-page-info", "missing-next-flag", "stuck-cursor", "unknown-event", "duplicate-event", "missing-merge", "null-repository"} {
		t.Run(scenario, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				body := prHistoryResponse()
				data := body["data"].(map[string]any)
				repository := data["repository"].(map[string]any)
				pr := repository["pullRequest"].(map[string]any)
				timeline := pr["timelineItems"].(map[string]any)
				nodes := timeline["nodes"].([]any)
				event := nodes[0].(map[string]any)
				switch scenario {
				case "graphql-error":
					body["errors"] = []any{map[string]any{"message": "private-content-must-not-leak"}}
				case "repository":
					repository["databaseId"] = 99
				case "pr-number":
					pr["number"] = 489
				case "missing-before":
					event["beforeCommit"] = nil
				case "missing-head-repo":
					pr["headRepository"] = nil
				case "missing-nodes":
					delete(timeline, "nodes")
				case "missing-page-info":
					delete(timeline, "pageInfo")
				case "missing-next-flag":
					delete(timeline["pageInfo"].(map[string]any), "hasNextPage")
				case "stuck-cursor":
					timeline["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "cursor-0"}
				case "unknown-event":
					event["__typename"] = "UnknownEvent"
				case "duplicate-event":
					timeline["nodes"] = append(nodes, event)
				case "missing-merge":
					pr["mergeCommit"] = nil
				case "null-repository":
					data["repository"] = nil
				}
				json.NewEncoder(w).Encode(body)
			})
			slot := client.slot(9)
			slot.token, slot.expires = "installation-test", time.Now().Add(time.Hour)
			_, err := client.PullRequestHeadHistory(context.Background(), 9, domain.GitHubRepository{ID: 10, FullName: "acme/project"}, 488, "cursor-0")
			if !errors.Is(err, domain.ErrIntegrity) || strings.Contains(err.Error(), "private-content") {
				t.Fatalf("invalid provider evidence accepted or leaked: %v", err)
			}
		})
	}
}

func TestPRHeadHistoryPaginationAndEmptyResult(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body := prHistoryResponse()
		pr := body["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"].(map[string]any)
		timeline := pr["timelineItems"].(map[string]any)
		if calls.Add(1) == 1 {
			if input.Variables["after"] != nil {
				t.Error("initial cursor must be null")
			}
			timeline["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "next-page"}
		} else {
			if input.Variables["after"] != "next-page" {
				t.Error("continuation cursor lost")
			}
			timeline["nodes"] = []any{}
			timeline["pageInfo"] = map[string]any{"hasNextPage": false, "endCursor": nil}
		}
		json.NewEncoder(w).Encode(body)
	})
	slot := client.slot(9)
	slot.token, slot.expires = "installation-test", time.Now().Add(time.Hour)
	repository := domain.GitHubRepository{ID: 10, FullName: "acme/project"}
	first, err := client.PullRequestHeadHistory(context.Background(), 9, repository, 488, "")
	if err != nil || !first.HasNextPage || first.EndCursor != "next-page" || calls.Load() != 1 {
		t.Fatalf("reader must return one bounded page: %+v %v calls=%d", first, err, calls.Load())
	}
	last, err := client.PullRequestHeadHistory(context.Background(), 9, repository, 488, first.EndCursor)
	if err != nil || last.HasNextPage || last.Rewrites == nil || len(last.Rewrites) != 0 || calls.Load() != 2 {
		t.Fatalf("valid empty terminal page rejected: %+v %v", last, err)
	}
}

func TestPRHeadHistoryRejectsInvalidScopeBeforeNetwork(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid scope made a provider request")
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, scope := range []struct {
		installation int64
		repository   domain.GitHubRepository
		number       int
		cursor       string
	}{
		{0, domain.GitHubRepository{ID: 10, FullName: "acme/project"}, 488, ""},
		{9, domain.GitHubRepository{FullName: "acme/project"}, 488, ""},
		{9, domain.GitHubRepository{ID: 10, FullName: "acme/project/elsewhere"}, 488, ""},
		{9, domain.GitHubRepository{ID: 10, FullName: "acme/project"}, 0, ""},
		{9, domain.GitHubRepository{ID: 10, FullName: "acme/project"}, 488, strings.Repeat("x", 4097)},
	} {
		if _, err := client.PullRequestHeadHistory(context.Background(), scope.installation, scope.repository, scope.number, scope.cursor); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid scope accepted: %+v %v", scope, err)
		}
	}
}

func TestPRHeadHistoryInvalidatesRevokedInstallation(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("private-provider-response"))
	})
	slot := client.slot(9)
	slot.token, slot.expires = "installation-test", time.Now().Add(time.Hour)
	_, err := client.PullRequestHeadHistory(context.Background(), 9, domain.GitHubRepository{ID: 10, FullName: "acme/project"}, 488, "")
	if !errors.Is(err, domain.ErrForbidden) || strings.Contains(err.Error(), "private-provider-response") {
		t.Fatalf("revoked access was accepted or leaked: %v", err)
	}
	if current := client.slot(9); current == slot || current.token != "" {
		t.Fatal("revoked installation token retained")
	}
}

func TestPRHeadHistoryGraphQLRateLimitCooldown(t *testing.T) {
	for _, scenario := range []string{"primary", "secondary", "typed", "success-last-point"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			reset := time.Now().Add(10 * time.Minute).Truncate(time.Second)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/installation/repositories" {
					json.NewEncoder(w).Encode(map[string]any{"repositories": []any{}})
					return
				}
				if r.Header.Get("Authorization") == "Bearer other-installation" || scenario == "success-last-point" {
					w.Header().Set("X-RateLimit-Remaining", "0")
					json.NewEncoder(w).Encode(prHistoryResponse())
					return
				}
				failure := map[string]any{"message": "private-provider-response"}
				switch scenario {
				case "primary":
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				case "secondary":
					w.Header().Set("Retry-After", "120")
				case "typed":
					failure["type"] = "RATE_LIMITED"
				}
				json.NewEncoder(w).Encode(map[string]any{"errors": []any{failure}})
			})
			slot := client.slot(9)
			slot.token, slot.expires = "installation-test", time.Now().Add(time.Hour)
			repository := domain.GitHubRepository{ID: 10, FullName: "acme/project"}
			if scenario == "success-last-point" {
				if _, err := client.PullRequestHeadHistory(context.Background(), 9, repository, 488, ""); err != nil {
					t.Fatalf("last successful response discarded: %v", err)
				}
				return
			}
			for attempt := 0; attempt < 2; attempt++ {
				_, err := client.PullRequestHeadHistory(context.Background(), 9, repository, 488, "")
				if err == nil || !strings.Contains(err.Error(), "rate limited") || strings.Contains(err.Error(), "private-provider-response") {
					t.Fatalf("rate limit not classified safely: %v", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("provider retried during cooldown: %d", calls.Load())
			}
			if scenario == "primary" {
				if _, err := client.Repositories(context.Background(), 9); err != nil || calls.Load() != 2 {
					t.Fatalf("GraphQL primary quota blocked same-installation REST: %v calls=%d", err, calls.Load())
				}
				if client.limits["installation:9:graphql"].Before(reset) {
					t.Fatal("provider reset was not respected")
				}
			} else if _, err := client.Repositories(context.Background(), 9); err == nil || calls.Load() != 1 {
				t.Fatalf("secondary quota did not defer shared installation: %v calls=%d", err, calls.Load())
			}
			priorCalls := calls.Load()
			other := client.slot(10)
			other.token, other.expires = "other-installation", time.Now().Add(time.Hour)
			if _, err := client.PullRequestHeadHistory(context.Background(), 10, repository, 488, ""); err != nil || calls.Load() != priorCalls+1 {
				t.Fatalf("one installation blocked another: %v", err)
			}
		})
	}
}
