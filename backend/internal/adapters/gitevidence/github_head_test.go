package gitevidence

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGitHubReadGitHeadExactBoundedReference(t *testing.T) {
	branch := "feature/name#escaped"
	commit := strings.Repeat("a", 40)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/repos/acme/project/git/ref/heads/"+branch || request.URL.RawQuery != "" || request.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("wrong exact reference request: %s %s", request.Method, request.URL)
		}
		json.NewEncoder(response).Encode(map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": commit}})
	}))
	defer server.Close()
	provider := NewGitHub(func() string { return "fixture-token" })
	provider.base = server.URL
	head, err := provider.ReadGitHead(context.Background(), "https://github.com/acme/project", branch)
	if err != nil || head.Ref != "refs/heads/"+branch || head.Commit != commit || requests != 1 {
		t.Fatal(head, err, requests)
	}
	if _, err := provider.ReadGitHead(context.Background(), "https://github.com/acme/project", "../bad"); err == nil || requests != 1 {
		t.Fatal("invalid branch issued a request")
	}
}

func TestGitHubReadGitHeadRejectsMissingRedirectAndWrongObjects(t *testing.T) {
	for _, scenario := range []string{"missing", "redirect", "prefix", "tag", "bad-sha", "array", "null", "rate-limit"} {
		t.Run(scenario, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				requests++
				ref, kind, hash := "refs/heads/feature", "commit", strings.Repeat("b", 40)
				switch scenario {
				case "missing":
					response.WriteHeader(http.StatusNotFound)
					return
				case "redirect":
					http.Redirect(response, request, "/redirected", http.StatusFound)
					return
				case "prefix":
					ref += "-other"
				case "tag":
					kind = "tag"
				case "bad-sha":
					hash = "branch-name"
				case "array":
					json.NewEncoder(response).Encode([]any{})
					return
				case "null":
					json.NewEncoder(response).Encode(nil)
					return
				case "rate-limit":
					response.Header().Set("Retry-After", "60")
					response.WriteHeader(http.StatusTooManyRequests)
					return
				}
				json.NewEncoder(response).Encode(map[string]any{"ref": ref, "object": map[string]string{"type": kind, "sha": hash}})
			}))
			defer server.Close()
			provider := NewGitHub(nil)
			provider.base = server.URL
			head, err := provider.ReadGitHead(context.Background(), "https://github.com/acme/project", "feature")
			if err == nil || head.Ref != "" || requests != 1 {
				t.Fatal("unproven reference accepted", head, err, requests)
			}
			if scenario == "rate-limit" {
				if _, _, err := provider.ListGitHeads(context.Background(), "https://github.com/acme/project", 1); err == nil || requests != 1 {
					t.Fatal("exact and paged reads did not share cooldown")
				}
			}
		})
	}
	if domain.ValidateBranchName("feature/name#escaped") != nil {
		t.Fatal("escape fixture uses invalid branch name")
	}
}
