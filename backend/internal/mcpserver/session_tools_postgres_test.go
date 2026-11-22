//go:build postgres

package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

// The session tools read through the independently assembled tool server:
// its own pool, no command service, and the same rule for who reads what.
func TestPGSessionToolsReadThroughTheIndependentServer(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("requires a disposable CXT_TEST_DSN")
	}
	t.Setenv("CXT_POSTGRES_DSN", dsn)
	t.Setenv("CXT_AUTH", "dev")
	t.Setenv("CXT_PUBLIC_URL", "https://cxthub.example")
	dir, err := filepath.Abs("../../../schemas/db/migrations")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CXT_MIGRATIONS_DIR", dir)
	ctx := context.Background()
	setup, cancel := context.WithTimeout(ctx, 30*time.Second)
	seed, err := store.NewPostgresStore(setup, dsn)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_, err = seed.ApplyPublicMigrations(setup, dir)
	seed.Close()
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	api, err := serverruntime.Open(ctx, "127.0.0.1:0", "", true)
	if err != nil {
		t.Fatal(err)
	}
	mcp, err := serverruntime.OpenMCP(ctx, "127.0.0.1:0", "", true)
	if err != nil {
		t.Fatal(err)
	}
	apiOpen := true
	defer func() {
		if apiOpen {
			api.Close()
		}
		mcp.Close()
	}()
	if mcp.Context != nil || mcp.Anchored == nil {
		t.Fatalf("the tool server holds a command service, or no session reads: %v %v", mcp.Context != nil, mcp.Anchored != nil)
	}

	uploader, _, err := api.Identity.Login(ctx, "dev:"+domain.NewID("uploader")+"@example.test", "uploader")
	if err != nil {
		t.Fatal(err)
	}
	teammate, _, err := api.Identity.Login(ctx, "dev:"+domain.NewID("teammate")+"@example.test", "teammate")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := api.Identity.CreateRepository(ctx, uploader, "session-tools")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(repository.ID)), RepositoryID: repository.ID, RemoteURL: "https://cxthub.example/" + uploader.Username + "/session-tools", DefaultBranch: "main"}
	if _, err = api.Store.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err = api.Store.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: teammate.ID, Role: domain.RoleMember, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	marker := "sessiontool" + strings.ToLower(strings.ReplaceAll(domain.NewID("m"), "_", ""))
	document := domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude},
		Events: []domain.CIREvent{
			{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "Plan the " + marker + " parser."}}},
			{Kind: domain.EventMessage, Seq: 1, Role: "assistant", Blocks: []domain.ContentBlock{{Type: "text", Text: "The parser is committed."}}},
			{Kind: domain.EventMessage, Seq: 2, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "What is left?"}}},
		},
	}}
	raw, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		t.Fatal(err)
	}
	document.Hash = domain.HashContent(raw)
	if _, err := api.Store.PutDoc(ctx, repo.ID, document); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("c", 40)
	own := inbound.WithRepositoryActor(ctx, uploader.ID)
	body := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 3,
		Content: &domain.AnchoredContent{Doc: document.Hash, Events: 3},
		Segments: []domain.AnchoredSegment{{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1, FirstEvent: 0, Events: 2},
			Commits: []domain.AnchoredCommit{{SHA: sha, Rule: domain.AnchoredLinkPrintedNumber, Subject: "Add the parser"}}}},
		Uncommitted: &domain.AnchoredUncommitted{AnchoredRange: domain.AnchoredRange{First: "m3", Last: "m3", FirstOrdinal: 2, LastOrdinal: 2, FirstEvent: 2, Events: 1},
			Places: []domain.AnchoredPlace{{Worktree: "app", Branch: "topic", Rule: domain.AnchoredPlaceRecorded}}},
	}
	if _, err := api.Context.PutAnchoredSession(own, repo.ID, domain.ProviderClaude, "session-1", body); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Context.PutAnchoredSummary(own, repo.ID, domain.ProviderClaude, "session-1", sha, domain.AnchoredSummaryBody{Summary: "Added the parser.", KeyFacts: []string{"It reads TOML."}}); err != nil {
		t.Fatal(err)
	}

	handler, err := Handler(mcp, mcp.PublicURL)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	token := func(user domain.User) string {
		pair, err := mcp.Identity.IssueMCPTokenPair(ctx, user.ID, domain.NewID("mcp_client_"), mcp.PublicURL+"/mcp")
		if err != nil {
			t.Fatal(err)
		}
		return pair.AccessToken
	}
	// call runs one tool and returns its text, and whether it was an error.
	call := func(bearer, tool string, args map[string]any) (string, bool) {
		t.Helper()
		args["repository"] = string(repo.ID)
		request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
		req, err := http.NewRequest(http.MethodPost, server.URL+"/mcp", strings.NewReader(string(request)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+bearer)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		answered, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s %v", tool, response.StatusCode, answered, err)
		}
		var envelope struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(answered, &envelope); err != nil || len(envelope.Result.Content) == 0 {
			t.Fatalf("%s: %s %v", tool, answered, err)
		}
		return envelope.Result.Content[0].Text, envelope.Result.IsError
	}
	// whole follows a tool's bounded fragments to its whole answer.
	whole := func(bearer, tool string, args map[string]any) string {
		t.Helper()
		answer := ""
		for {
			text, failed := call(bearer, tool, args)
			var page struct {
				Fragment string `json:"json_fragment"`
				Next     string `json:"next_cursor"`
			}
			if failed || json.Unmarshal([]byte(text), &page) != nil {
				t.Fatalf("%s: %s", tool, text)
			}
			answer += page.Fragment
			if page.Next == "" {
				return answer
			}
			args["cursor"] = page.Next
		}
	}
	read := func() {
		t.Helper()
		mine, theirs := token(uploader), token(teammate)
		if listed := whole(mine, "session_list", map[string]any{}); !strings.Contains(listed, `"session":"session-1"`) || !strings.Contains(listed, `"own":true`) || !strings.Contains(listed, sha) || !strings.Contains(listed, `"uncommitted_on":["topic"]`) {
			t.Fatalf("the uploader's list: %s", listed)
		}
		conversation, failed := call(mine, "session_read", map[string]any{"provider": "claude", "session": "session-1"})
		if failed || !strings.Contains(conversation, marker) || !strings.Contains(conversation, "What is left?") || strings.Contains(conversation, string(document.Hash)) {
			t.Fatalf("the uploader's conversation: %s", conversation)
		}
		if part, failed := call(mine, "session_read", map[string]any{"provider": "claude", "session": "session-1", "segment": "rest"}); failed || !strings.Contains(part, "What is left?") || strings.Contains(part, marker) {
			t.Fatalf("the part after the last commit: %s", part)
		}
		if commit := whole(mine, "session_commit", map[string]any{"commit": sha}); !strings.Contains(commit, `"session":"session-1"`) || !strings.Contains(commit, `"events":[0,2]`) || !strings.Contains(commit, "Added the parser.") || !strings.Contains(commit, "It reads TOML.") {
			t.Fatalf("the uploader's commit: %s", commit)
		}
		if along := whole(mine, "session_summaries", map[string]any{"branch": "main"}); !strings.Contains(along, `"summaries":[]`) {
			t.Fatalf("along a branch of a remote that was never read: %s", along)
		}
		if found := whole(mine, "session_search", map[string]any{"query": marker}); !strings.Contains(found, `"kind":"segment"`) || !strings.Contains(found, `"session_id":"session-1"`) {
			t.Fatalf("the uploader's search: %s", found)
		}
		// Nothing of the session is pushed: a teammate reads nothing of it.
		if listed := whole(theirs, "session_list", map[string]any{}); !strings.Contains(listed, `"sessions":[]`) {
			t.Fatalf("a teammate's list: %s", listed)
		}
		if text, failed := call(theirs, "session_read", map[string]any{"provider": "claude", "session": "session-1"}); !failed || strings.Contains(text, marker) {
			t.Fatalf("a teammate read the conversation: %s", text)
		}
		if commit := whole(theirs, "session_commit", map[string]any{"commit": sha}); !strings.Contains(commit, `"segments":[]`) || strings.Contains(commit, "Added the parser.") {
			t.Fatalf("a teammate's commit: %s", commit)
		}
		if found := whole(theirs, "session_search", map[string]any{"query": marker}); strings.Contains(found, "session-1") {
			t.Fatalf("a teammate's search: %s", found)
		}
	}
	read()
	// The tool server keeps reading with the API server gone.
	api.Close()
	apiOpen = false
	read()
}
