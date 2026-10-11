package githubapp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMergedPRDiscoveryUsesStableCreationPages(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if request.URL.Path != "/repositories/10/pulls" || query.Get("state") != "all" || query.Get("sort") != "created" || query.Get("direction") != "asc" || query.Get("per_page") != "100" || query.Has("base") {
			t.Errorf("mutable pagination query: %s", request.URL.String())
		}
		if query.Get("page") == "1" {
			open := make([]map[string]any, 100)
			for index := range open {
				open[index] = map[string]any{"number": index + 1, "merged_at": nil}
			}
			json.NewEncoder(w).Encode(open)
			return
		}
		if query.Get("page") != "2" {
			t.Errorf("unexpected page: %s", query.Get("page"))
		}
		json.NewEncoder(w).Encode([]any{map[string]any{
			"number": 101, "merged_at": "2026-10-10T10:00:00Z", "merge_commit_sha": strings.Repeat("d", 40),
			"head": map[string]any{"sha": strings.Repeat("c", 40), "ref": "feature", "repo": map[string]any{"id": 10}},
			"base": map[string]any{"ref": "main", "repo": map[string]any{"id": 10}},
		}})
	})
	slot := client.slot(9)
	slot.token, slot.expires = "installation-test", time.Now().Add(time.Hour)
	first, more, err := client.MergedPullRequests(context.Background(), 9, domain.GitHubRepository{ID: 10}, 1)
	if err != nil || !more || len(first) != 0 {
		t.Fatalf("non-merged page incorrectly terminal: %+v %v %v", first, more, err)
	}
	second, more, err := client.MergedPullRequests(context.Background(), 9, domain.GitHubRepository{ID: 10}, 2)
	if err != nil || more || len(second) != 1 || second[0].Number != 101 {
		t.Fatalf("later merged PR lost: %+v %v %v", second, more, err)
	}
}
