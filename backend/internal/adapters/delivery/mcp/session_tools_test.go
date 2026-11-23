package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// sessionQueries answers the reads of the session structure from what a test
// gave it, and records who asked for what.
type sessionQueries struct {
	sessions  []domain.AnchoredSessionSummary
	session   domain.AnchoredSession
	segments  []domain.AnchoredCommitSegment
	summaries []domain.AnchoredSummary
	memory    domain.AnchoredMemory
	found     domain.AnchoredSearchResult
	page      domain.AnchoredFragmentPage
	err       error
	asked     []string
}

func (q *sessionQueries) record(ctx context.Context, what string, parts ...any) {
	actor, _ := inbound.RepositoryActor(ctx)
	q.asked = append(q.asked, strings.TrimSpace(actor+" "+what+" "+fmt.Sprint(parts...)))
}

func (q *sessionQueries) ListRepos(context.Context, string) ([]domain.Repo, error) {
	return nil, nil
}

func (q *sessionQueries) GetAnchoredSession(ctx context.Context, _ domain.ContentHash, provider domain.ProviderKind, id string) (domain.AnchoredSession, error) {
	q.record(ctx, "get", provider, "/", id)
	return q.session, q.err
}

func (q *sessionQueries) ListAnchoredSessions(ctx context.Context, _ domain.ContentHash) ([]domain.AnchoredSessionSummary, error) {
	q.record(ctx, "list")
	return q.sessions, q.err
}

func (q *sessionQueries) AnchoredCommitSegments(ctx context.Context, _ domain.ContentHash, sha string) ([]domain.AnchoredCommitSegment, error) {
	q.record(ctx, "commit", sha[:4])
	return q.segments, q.err
}

func (q *sessionQueries) ListAnchoredSummaries(ctx context.Context, _ domain.ContentHash, commits []string) ([]domain.AnchoredSummary, error) {
	q.record(ctx, "summaries", len(commits))
	return q.summaries, q.err
}

func (q *sessionQueries) ReadAnchoredMemory(ctx context.Context, _ domain.ContentHash, branch string, local []string, limit int) (domain.AnchoredMemory, error) {
	q.record(ctx, "memory", branch, ":", len(local), ":", limit)
	return q.memory, q.err
}

func (q *sessionQueries) SearchAnchored(ctx context.Context, _ domain.ContentHash, query string) (domain.AnchoredSearchResult, error) {
	q.record(ctx, "search", query)
	return q.found, q.err
}

func (q *sessionQueries) ReadAnchoredSessionFragments(ctx context.Context, _ domain.ContentHash, provider domain.ProviderKind, id string, index, offset, limit, budget int) (domain.AnchoredFragmentPage, error) {
	q.record(ctx, "read", provider, "/", id, " ", index, ":", offset, ":", limit, ":", budget)
	return q.page, q.err
}

func sessionToolFixture(t *testing.T) (*Server, *sessionQueries, domain.User, domain.Repo) {
	t.Helper()
	ctx := systemTestContext()
	storage := store.NewFSStore(t.TempDir())
	user := domain.User{ID: "session-reader", Email: "reader@example.test", Name: "Reader", Username: "session-reader"}
	identity := app.NewIdentityService(fixedVerifier{user: user}, storage)
	if _, _, err := identity.Login(ctx, "idp-token", "test"); err != nil {
		t.Fatal(err)
	}
	repository, err := identity.CreateRepository(ctx, user, "Sessions")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: pageHash(1), RepositoryID: repository.ID}
	server, err := NewServer(fakeContextBackend{repos: []domain.Repo{repo}}, identity, storage, "https://cxthub.test")
	if err != nil {
		t.Fatal(err)
	}
	queries := &sessionQueries{}
	server.SetAnchoredQueries(queries)
	return server, queries, user, repo
}

// sessionToolAnswer calls a tool and follows its bounded fragments to the
// whole JSON answer.
func sessionToolAnswer(t *testing.T, server *Server, user domain.User, tool string, args map[string]any) (map[string]any, int) {
	t.Helper()
	whole, pages := "", 0
	for {
		raw, _ := json.Marshal(args)
		result, err := server.runTool(systemTestContext(), user, tool, raw)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		// A piece holds at most the page budget of the answer; quoting it as a
		// JSON string can double that.
		if len(result) > 2*pageBytes+1024 {
			t.Fatalf("%s answered %d bytes at once", tool, len(result))
		}
		var page struct {
			Notice   string `json:"notice"`
			Offset   int    `json:"byte_offset"`
			Fragment string `json:"json_fragment"`
			Complete bool   `json:"page_complete"`
			Next     string `json:"next_cursor"`
		}
		if err := json.Unmarshal([]byte(result), &page); err != nil || page.Notice != sessionNotice || page.Offset != len(whole) || page.Complete != (page.Next == "") || len(page.Fragment) > pageBytes {
			t.Fatalf("%s page: %s %v", tool, result, err)
		}
		whole += page.Fragment
		pages++
		if page.Next == "" {
			break
		}
		args["cursor"] = page.Next
	}
	delete(args, "cursor")
	var answer map[string]any
	if err := json.Unmarshal([]byte(whole), &answer); err != nil {
		t.Fatalf("%s does not reassemble: %v\n%s", tool, err, whole)
	}
	return answer, pages
}

func TestSessionToolsAreDeclaredReadOnlyAndNameNoHash(t *testing.T) {
	declared := map[string]map[string]any{}
	for _, tool := range toolDefinitions() {
		declared[tool["name"].(string)] = tool
	}
	for _, name := range []string{"session_list", "session_read", "session_commit", "session_summaries", "session_search"} {
		tool, ok := declared[name]
		if !ok {
			t.Fatalf("%s is not declared", name)
		}
		properties := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		if _, has := properties["cursor"]; !has || properties["repository"] == nil {
			t.Fatalf("%s lacks a cursor or a repository: %v", name, properties)
		}
		for _, hashed := range []string{"ref", "memory_hash", "doc", "snapshot"} {
			if _, has := properties[hashed]; has {
				t.Fatalf("%s takes %s", name, hashed)
			}
		}
		if annotations := tool["annotations"].(map[string]any); annotations["readOnlyHint"] != true {
			t.Fatalf("%s is not read-only: %v", name, annotations)
		}
	}
	if !strings.Contains(serverInstructions, "session_list") || !strings.Contains(serverInstructions, "session_summaries") {
		t.Fatal("the server's instructions do not name the session tools")
	}
}

func TestSessionListToolListsWhatTheCallerMayRead(t *testing.T) {
	server, queries, user, repo := sessionToolFixture(t)
	at := func(minute int) *time.Time {
		moment := time.Date(2031, 1, 2, 3, minute, 0, 0, time.UTC)
		return &moment
	}
	pushed := true
	queries.sessions = []domain.AnchoredSessionSummary{
		{Provider: domain.ProviderClaude, SessionID: "older", StartedAt: at(1), Segments: 1, Content: true,
			Commits: []domain.AnchoredSummaryCommit{{SHA: strings.Repeat("a", 40), SegmentNo: 1, Subject: "Add the parser", OnRemote: &pushed}}},
		{Provider: domain.ProviderCodex, SessionID: "theirs", StartedAt: at(9), Shared: true, UploaderName: "bob", Segments: 1},
		{Provider: domain.ProviderClaude, SessionID: "archived", StartedAt: at(20), ArchivedAt: at(30)},
		{Provider: domain.ProviderClaude, SessionID: "undated", Uncommitted: &domain.AnchoredSummaryUncommitted{Places: []domain.AnchoredPlace{{Branch: "topic"}, {Branch: "topic"}, {Branch: ""}}}},
	}
	answer, pages := sessionToolAnswer(t, server, user, "session_list", map[string]any{"repository": string(repo.ID)})
	listed := answer["sessions"].([]any)
	var ids []string
	for _, entry := range listed {
		ids = append(ids, entry.(map[string]any)["session"].(string))
	}
	if pages != 1 || !reflect.DeepEqual(ids, []string{"theirs", "older", "undated"}) {
		t.Fatalf("sessions: %v in %d pages", ids, pages)
	}
	theirs, older, undated := listed[0].(map[string]any), listed[1].(map[string]any), listed[2].(map[string]any)
	if theirs["own"] != false || theirs["uploader"] != "bob" || older["own"] != true || older["conversation"] != true {
		t.Fatalf("whose sessions: %v %v", theirs, older)
	}
	commit := older["commits"].([]any)[0].(map[string]any)
	if commit["commit"] != strings.Repeat("a", 40) || commit["segment"] != float64(1) || commit["subject"] != "Add the parser" || commit["on_remote"] != true {
		t.Fatalf("commit: %v", commit)
	}
	if !reflect.DeepEqual(undated["uncommitted_on"], []any{"topic"}) {
		t.Fatalf("uncommitted: %v", undated)
	}
	if !reflect.DeepEqual(queries.asked, []string{"session-reader list"}) {
		t.Fatalf("asked: %v", queries.asked)
	}

	// A long list comes in bounded pieces that reassemble.
	queries.sessions = nil
	for index := 0; index < 400; index++ {
		queries.sessions = append(queries.sessions, domain.AnchoredSessionSummary{Provider: domain.ProviderClaude, SessionID: fmt.Sprintf("session-%03d", index), StartedAt: at(index % 50)})
	}
	answer, pages = sessionToolAnswer(t, server, user, "session_list", map[string]any{"repository": string(repo.ID)})
	if pages < 2 || len(answer["sessions"].([]any)) != 400 {
		t.Fatalf("a long list: %d sessions in %d pages", len(answer["sessions"].([]any)), pages)
	}
	// The list changed between two pieces: the reader starts again.
	raw, _ := json.Marshal(map[string]any{"repository": string(repo.ID)})
	first, err := server.runTool(systemTestContext(), user, "session_list", raw)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Next string `json:"next_cursor"`
	}
	if json.Unmarshal([]byte(first), &page) != nil || page.Next == "" {
		t.Fatalf("first piece: %s", first)
	}
	queries.sessions = queries.sessions[:399]
	raw, _ = json.Marshal(map[string]any{"repository": string(repo.ID), "cursor": page.Next})
	if _, err := server.runTool(systemTestContext(), user, "session_list", raw); err == nil || !strings.Contains(err.Error(), "restart without cursor") {
		t.Fatalf("a changed list: %v", err)
	}
	// A cursor of one tool is not one of another.
	if _, err := server.runTool(systemTestContext(), user, "session_search", []byte(`{"repository":"`+string(repo.ID)+`","query":"parser","cursor":"`+page.Next+`"}`)); err == nil {
		t.Fatal("a cursor of session_list continued session_search")
	}
}

func TestSessionReadToolReadsWithinThePartAndWhatIsOpen(t *testing.T) {
	server, queries, user, repo := sessionToolFixture(t)
	queries.session = domain.AnchoredSession{Body: domain.AnchoredSessionBody{
		Segments: []domain.AnchoredSegment{
			{AnchoredRange: domain.AnchoredRange{FirstEvent: 0, Events: 2}},
			{AnchoredRange: domain.AnchoredRange{FirstEvent: 2, Events: 3}},
			{Hidden: true},
		},
	}}
	fragment := func(index int) domain.DocEventFragment {
		return domain.DocEventFragment{Index: index, Complete: true, JSON: fmt.Sprintf(`{"seq":%d}`, index)}
	}
	read := func(args map[string]any) (map[string]any, error) {
		args["repository"], args["provider"], args["session"] = string(repo.ID), "claude", "session-1"
		raw, _ := json.Marshal(args)
		result, err := server.runTool(systemTestContext(), user, "session_read", raw)
		if err != nil {
			return nil, err
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(result), &out); err != nil || out["notice"] != sessionNotice {
			t.Fatalf("answer: %s %v", result, err)
		}
		if strings.Contains(result, "sha256:") {
			t.Fatalf("a hash was answered: %s", result)
		}
		return out, nil
	}

	// The whole conversation: from the first event, twelve events at a time.
	queries.page = domain.AnchoredFragmentPage{Fragments: []domain.DocEventFragment{fragment(0), fragment(1)}, Total: 9, Open: [][2]int{{0, 2}, {5, 9}}, NextIndex: 5}
	out, err := read(map[string]any{})
	if err != nil || len(out["fragments"].([]any)) != 2 || out["total_events"] != float64(9) || out["next_cursor"] == "" || !reflect.DeepEqual(out["open_events"], []any{[]any{float64(0), float64(2)}, []any{float64(5), float64(9)}}) {
		t.Fatalf("the first page: %v %v", out, err)
	}
	if last := queries.asked[len(queries.asked)-1]; last != fmt.Sprintf("session-reader read claude/session-1 0:0:12:%d", pageBytes) {
		t.Fatalf("asked: %s", last)
	}
	// The cursor goes on where the page said, past the part that is not open.
	queries.page = domain.AnchoredFragmentPage{Fragments: []domain.DocEventFragment{fragment(5)}, Total: 9, Open: [][2]int{{0, 2}, {5, 9}}, NextIndex: -1}
	out, err = read(map[string]any{"cursor": out["next_cursor"], "events": 3})
	if err != nil || out["next_cursor"] != "" || len(out["fragments"].([]any)) != 1 {
		t.Fatalf("the second page: %v %v", out, err)
	}
	if last := queries.asked[len(queries.asked)-1]; last != fmt.Sprintf("session-reader read claude/session-1 5:0:3:%d", pageBytes) {
		t.Fatalf("asked: %s", last)
	}

	// One segment: only its events, however many the reader returns.
	queries.asked = nil
	queries.page = domain.AnchoredFragmentPage{Fragments: []domain.DocEventFragment{fragment(2), fragment(3), fragment(4), fragment(5)}, Total: 9, Open: [][2]int{{0, 9}}, NextIndex: 5}
	out, err = read(map[string]any{"segment": "2", "events": 50})
	if err != nil || len(out["fragments"].([]any)) != 3 || out["next_cursor"] != "" || !reflect.DeepEqual(out["events"], []any{float64(2), float64(5)}) {
		t.Fatalf("one segment: %v %v", out, err)
	}
	if want := []string{"session-reader get claude/session-1", fmt.Sprintf("session-reader read claude/session-1 2:0:3:%d", pageBytes)}; !reflect.DeepEqual(queries.asked, want) {
		t.Fatalf("asked: %v", queries.asked)
	}
	// A page in the middle of the segment hands on a cursor inside it.
	queries.page = domain.AnchoredFragmentPage{Fragments: []domain.DocEventFragment{{Index: 2, JSON: `{"seq"`}}, Total: 9, Open: [][2]int{{0, 9}}, NextIndex: 2, NextOffset: 6}
	out, err = read(map[string]any{"segment": "2"})
	if err != nil || out["next_cursor"] == "" {
		t.Fatalf("a piece of an event: %v %v", out, err)
	}
	queries.page = domain.AnchoredFragmentPage{Fragments: []domain.DocEventFragment{{Index: 2, Offset: 6, Complete: true, JSON: `:2}`}}, Total: 9, Open: [][2]int{{0, 9}}, NextIndex: 3}
	if out, err = read(map[string]any{"segment": "2", "cursor": out["next_cursor"]}); err != nil || out["next_cursor"] == "" {
		t.Fatalf("the rest of the event: %v %v", out, err)
	}
	if last := queries.asked[len(queries.asked)-1]; last != fmt.Sprintf("session-reader read claude/session-1 2:6:3:%d", pageBytes) {
		t.Fatalf("asked: %s", last)
	}
	// The same cursor does not continue another part.
	if _, err := read(map[string]any{"segment": "1", "cursor": out["next_cursor"]}); err == nil {
		t.Fatal("a cursor of one segment continued another")
	}

	for name, args := range map[string]map[string]any{
		"a segment that is not pushed":   {"segment": "3"},
		"a segment the session has not":  {"segment": "4"},
		"a segment that is not a number": {"segment": "last"},
		"no part after the last commit":  {"segment": "rest"},
	} {
		if _, err := read(args); err == nil {
			t.Fatalf("%s was read", name)
		}
	}
	raw, _ := json.Marshal(map[string]any{"repository": string(repo.ID), "provider": "other", "session": "session-1"})
	if _, err := server.runTool(systemTestContext(), user, "session_read", raw); err == nil {
		t.Fatal("an unknown tool's session was read")
	}
	queries.err = domain.ErrNotFound
	if _, err := read(map[string]any{}); err == nil {
		t.Fatal("a session the caller may not read was answered")
	}
}

func TestSessionCommitSummariesAndSearchTools(t *testing.T) {
	server, queries, user, repo := sessionToolFixture(t)
	sha := strings.Repeat("a", 40)
	started := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	body := domain.AnchoredSummaryBody{Summary: "Added the parser.", KeyFacts: []string{"It reads TOML."}, OpenTasks: []string{"Document it."}, TasksAuthoritative: true}
	queries.segments = []domain.AnchoredCommitSegment{
		{Provider: domain.ProviderClaude, SessionID: "session-1", SegmentNo: 2, Rule: "printed-number",
			Segment: domain.AnchoredSegment{AnchoredRange: domain.AnchoredRange{FirstEvent: 4, Events: 3, Commands: 2, StartedAt: &started}}},
		{Provider: domain.ProviderCodex, SessionID: "session-2", SegmentNo: 1, Rule: "same-folder-and-time"},
	}
	queries.summaries = []domain.AnchoredSummary{{Provider: domain.ProviderClaude, SessionID: "session-1", Commit: sha, Body: body, SegmentNo: 2}}
	answer, _ := sessionToolAnswer(t, server, user, "session_commit", map[string]any{"repository": string(repo.ID), "commit": sha})
	segments := answer["segments"].([]any)
	first, second := segments[0].(map[string]any), segments[1].(map[string]any)
	if answer["commit"] != sha || len(segments) != 2 || first["session"] != "session-1" || first["segment"] != float64(2) || first["linked_by"] != "printed-number" ||
		!reflect.DeepEqual(first["events"], []any{float64(4), float64(7)}) || first["started_at"] == nil {
		t.Fatalf("commit: %v", answer)
	}
	if summary := first["summary"].(map[string]any); summary["summary"] != "Added the parser." || summary["open_tasks_from_tool"] != true || !reflect.DeepEqual(summary["key_facts"], []any{"It reads TOML."}) {
		t.Fatalf("summary: %v", summary)
	}
	if _, has := second["summary"]; has {
		t.Fatalf("a segment without a summary was given one: %v", second)
	}
	if want := []string{"session-reader commit aaaa", "session-reader summaries 1"}; !reflect.DeepEqual(queries.asked, want) {
		t.Fatalf("asked: %v", queries.asked)
	}
	for _, commit := range []string{"", "abc1234", strings.Repeat("A", 40)} {
		raw, _ := json.Marshal(map[string]any{"repository": string(repo.ID), "commit": commit})
		if _, err := server.runTool(systemTestContext(), user, "session_commit", raw); err == nil {
			t.Fatalf("commit %q was accepted", commit)
		}
	}

	// A long summary comes in pieces.
	queries.memory = domain.AnchoredMemory{Branch: "main", Head: sha, Walked: 7, Summaries: []domain.AnchoredSummary{
		{Provider: domain.ProviderClaude, SessionID: "session-1", Commit: sha, SegmentNo: 2, UploaderName: "alice", UpdatedAt: started,
			Body: domain.AnchoredSummaryBody{Summary: strings.Repeat("A long account of the work. ", 2000), OpenTasks: []string{"Document it."}}},
	}}
	queries.asked = nil
	answer, pages := sessionToolAnswer(t, server, user, "session_summaries", map[string]any{"repository": string(repo.ID), "branch": "topic"})
	summary := answer["summaries"].([]any)[0].(map[string]any)
	if pages < 2 || answer["branch"] != "main" || answer["commits_walked"] != float64(7) || summary["uploader"] != "alice" || summary["commit"] != sha ||
		summary["summary"].(map[string]any)["open_tasks_from_tool"] != false || len(summary["summary"].(map[string]any)["summary"].(string)) != 2000*28 {
		t.Fatalf("summaries in %d pages: %.300v", pages, answer)
	}
	if queries.asked[0] != "session-reader memory topic:0:5" {
		t.Fatalf("asked: %v", queries.asked)
	}
	queries.asked = nil
	sessionToolAnswer(t, server, user, "session_summaries", map[string]any{"repository": string(repo.ID), "limit": 99})
	if queries.asked[0] != fmt.Sprintf("session-reader memory :0:%d", domain.AnchoredMemoryLimit) {
		t.Fatalf("asked with a large limit: %v", queries.asked)
	}

	event := 4
	queries.found = domain.AnchoredSearchResult{Truncated: true, Hits: []domain.AnchoredSearchHit{{Kind: domain.AnchoredHitSegment, Snippet: "the parser", Provider: domain.ProviderClaude, SessionID: "session-1", SegmentNo: 2, Event: &event}}}
	queries.asked = nil
	answer, _ = sessionToolAnswer(t, server, user, "session_search", map[string]any{"repository": string(repo.ID), "query": "parser"})
	if hits := answer["hits"].([]any); answer["truncated"] != true || len(hits) != 1 || hits[0].(map[string]any)["session_id"] != "session-1" || hits[0].(map[string]any)["event"] != float64(4) {
		t.Fatalf("search: %v", answer)
	}
	if !reflect.DeepEqual(queries.asked, []string{"session-reader search parser"}) {
		t.Fatalf("asked: %v", queries.asked)
	}
}

func TestSessionToolsNeedARepositoryTheCallerMayRead(t *testing.T) {
	server, queries, user, repo := sessionToolFixture(t)
	for _, tool := range []string{"session_list", "session_read", "session_commit", "session_summaries", "session_search"} {
		raw, _ := json.Marshal(map[string]any{"repository": string(repo.ID), "provider": "claude", "session": "session-1", "commit": strings.Repeat("a", 40), "query": "parser"})
		if _, err := server.runTool(systemTestContext(), domain.User{ID: "outsider"}, tool, raw); err == nil {
			t.Fatalf("%s answered an account outside the repository", tool)
		}
		if _, err := server.runTool(systemTestContext(), user, tool, []byte(`{}`)); err == nil {
			t.Fatalf("%s answered without a repository", tool)
		}
	}
	if len(queries.asked) != 0 {
		t.Fatalf("a refused call reached the reads: %v", queries.asked)
	}
	server.SetAnchoredQueries(nil)
	raw, _ := json.Marshal(map[string]any{"repository": string(repo.ID)})
	if _, err := server.runTool(systemTestContext(), user, "session_list", raw); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("without the reads: %v", err)
	}
}
