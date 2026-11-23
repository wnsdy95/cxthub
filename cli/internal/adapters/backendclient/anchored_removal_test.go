package backendclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func TestRemovalsAreListedMadeAndTakenBack(t *testing.T) {
	repo := string(domain.HashContent([]byte("synthetic repository")))
	var method, path, auth string
	status, answer := http.StatusOK, `[]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	defer server.Close()
	client := NewBackendClient(func() string { return server.URL + "/api/v1" }, func() string { return "synthetic-token" }, domain.TeamIdentity{})
	ctx := context.Background()

	// The caller's own removals.
	answer = `[{"provider":"claude","session_id":"session-1","removed_at":"2031-01-02T03:04:05Z","by_you":true},{"provider":"codex","session_id":"thread-1","removed_at":"2031-01-03T03:04:05Z","by_you":false}]`
	listed, err := client.ListRemovedAnchoredSessions(ctx, repo)
	want := []domain.AnchoredSessionRemoval{{Provider: domain.ProviderClaude, SessionID: "session-1", RemovedAt: "2031-01-02T03:04:05Z", ByYou: true},
		{Provider: domain.ProviderCodex, SessionID: "thread-1", RemovedAt: "2031-01-03T03:04:05Z"}}
	if err != nil || !reflect.DeepEqual(listed, want) || method != http.MethodGet || path != "/api/v1/repos/"+repo+"/anchored-removals" || auth != "Bearer synthetic-token" {
		t.Fatalf("list: %+v %v (%s %s)", listed, err, method, path)
	}
	// A server from before removals has no such route: no removal.
	status, answer = http.StatusNotFound, `{"error":{"code":"not_found","message":"no such route"}}`
	if listed, err := client.ListRemovedAnchoredSessions(ctx, repo); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("a server without the route: %#v %v", listed, err)
	}
	// Any other refusal is one.
	status = http.StatusForbidden
	if _, err := client.ListRemovedAnchoredSessions(ctx, repo); err == nil {
		t.Fatal("a refusal was read as an empty list")
	}

	// Removing answers what was removed, the session asked for first.
	status, answer = http.StatusOK, `{"removed":[{"provider":"claude","session_id":"session-1","removed_at":"2031-01-02T03:04:05Z","by_you":true},{"provider":"claude","session_id":"sub-1","removed_at":"2031-01-02T03:04:05Z","by_you":true}]}`
	removed, err := client.RemoveAnchoredSession(ctx, repo, domain.ProviderClaude, "session-1")
	if err != nil || len(removed) != 2 || removed[1].SessionID != "sub-1" || method != http.MethodDelete || path != "/api/v1/repos/"+repo+"/anchored-sessions/claude/session-1" {
		t.Fatalf("remove: %+v %v (%s %s)", removed, err, method, path)
	}
	status, answer = http.StatusNotFound, `{"error":{"code":"not_found","message":"not found"}}`
	if _, err := client.RemoveAnchoredSession(ctx, repo, domain.ProviderClaude, "session-1"); err == nil {
		t.Fatal("removing a session that is not stored succeeded")
	}

	// Taking a removal back.
	status, answer = http.StatusNoContent, ``
	if err := client.AllowAnchoredSession(ctx, repo, domain.ProviderCodex, "thread-1"); err != nil || method != http.MethodDelete || path != "/api/v1/repos/"+repo+"/anchored-sessions/codex/thread-1/removal" {
		t.Fatalf("allow: %v (%s %s)", err, method, path)
	}
	status, answer = http.StatusForbidden, `{"error":{"code":"forbidden","message":"forbidden"}}`
	if err := client.AllowAnchoredSession(ctx, repo, domain.ProviderCodex, "thread-1"); err == nil {
		t.Fatal("a refused allow succeeded")
	}

	// What cannot name a session is not sent.
	method = ""
	for name, call := range map[string]func() error{
		"a tool that is not one": func() error {
			_, err := client.RemoveAnchoredSession(ctx, repo, "other", "session-1")
			return err
		},
		"an identifier that is a path": func() error { return client.AllowAnchoredSession(ctx, repo, domain.ProviderClaude, "../escape") },
	} {
		if err := call(); !errors.Is(err, domain.ErrInvalidRef) || method != "" {
			t.Fatalf("%s: %v %q", name, err, method)
		}
	}
	if _, err := client.ListRemovedAnchoredSessions(ctx, "not-a-repository"); err == nil || method != "" {
		t.Fatalf("a repository ID that is not one: %v %q", err, method)
	}
}

func TestTheServerIsReadAndAskedForTheDiagnosisAndItsFixes(t *testing.T) {
	repo := string(domain.HashContent([]byte("synthetic repository")))
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var method, path, query, sent string
	status, answer := http.StatusOK, `[]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query = r.Method, r.URL.EscapedPath(), r.URL.RawQuery
		raw := make([]byte, 256)
		n, _ := r.Body.Read(raw)
		sent = string(raw[:n])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	defer server.Close()
	client := NewBackendClient(func() string { return server.URL + "/api/v1" }, func() string { return "synthetic-token" }, domain.TeamIdentity{})
	ctx := context.Background()

	answer = `[{"provider":"claude","session_id":"session-1","revision":3,"body_hash":"sha256:b","records":7,"segments":2,"content":true,"uploader":"account","updated_at":"2031-01-02T03:04:05Z","archived_at":"2031-01-03T03:04:05Z","commits":[{"sha":"` + sha + `","segment_no":1,"rule":"printed-number","on_remote":true},{"sha":"` + sha + `","segment_no":2,"rule":"printed-number","copies":["` + sha + `"]}]},{"provider":"codex","session_id":"thread-1","revision":1,"body_hash":"sha256:c","records":1,"segments":0,"shared":true,"commits":[]}]`
	sessions, err := client.ListAnchoredSessions(ctx, repo)
	if err != nil || len(sessions) != 2 || method != http.MethodGet || path != "/api/v1/repos/"+repo+"/anchored-sessions" {
		t.Fatalf("list: %+v %v (%s %s)", sessions, err, method, path)
	}
	first := sessions[0]
	if first.Revision != 3 || first.BodyHash != "sha256:b" || first.Records != 7 || first.Segments != 2 || first.Shared || first.ArchivedAt != "2031-01-03T03:04:05Z" ||
		len(first.Commits) != 2 || first.Commits[0].OnRemote == nil || !*first.Commits[0].OnRemote || first.Commits[1].OnRemote != nil || len(first.Commits[1].Copies) != 1 || !sessions[1].Shared {
		t.Fatalf("sessions: %+v", sessions)
	}

	answer = `[{"provider":"claude","session_id":"session-1","commit":"` + sha + `","body":{"summary":"Added the parser.","key_facts":[],"open_tasks":[]},"segment_no":1}]`
	summaries, err := client.ListAnchoredCommitSummaries(ctx, repo, sha)
	if err != nil || len(summaries) != 1 || summaries[0].SessionID != "session-1" || path != "/api/v1/repos/"+repo+"/anchored-commits/"+sha+"/summaries" {
		t.Fatalf("summaries: %+v %v (%s)", summaries, err, path)
	}
	if _, err := client.ListAnchoredCommitSummaries(ctx, repo, "abc1234"); !errors.Is(err, domain.ErrInvalidRef) {
		t.Fatalf("a short commit number: %v", err)
	}

	// When the server last read GitHub; a repository it never read has no time.
	answer = `{"default_branch":"main","synced_at":"2031-01-02T03:04:05Z","branches":[],"pulls":[],"commits":[]}`
	if at, err := client.ReadRemoteGitSyncedAt(ctx, repo); err != nil || at != "2031-01-02T03:04:05Z" || path != "/api/v1/repos/"+repo+"/remote-git" || query != "limit=1" {
		t.Fatalf("synced at: %q %v (%s?%s)", at, err, path, query)
	}
	answer = `{"branches":[],"pulls":[],"commits":[]}`
	if at, err := client.ReadRemoteGitSyncedAt(ctx, repo); err != nil || at != "" {
		t.Fatalf("never read: %q %v", at, err)
	}

	status, answer = http.StatusAccepted, `{"requested":true}`
	if err := client.RequestRemoteGitRefresh(ctx, repo); err != nil || method != http.MethodPost || path != "/api/v1/repos/"+repo+"/remote-git/refresh" {
		t.Fatalf("refresh: %v (%s %s)", err, method, path)
	}
	status, answer = http.StatusOK, `{"archived":true}`
	if err := client.SetAnchoredSessionArchived(ctx, repo, domain.ProviderClaude, "session-1", true); err != nil || method != http.MethodPut ||
		path != "/api/v1/repos/"+repo+"/anchored-sessions/claude/session-1/archive" || sent != `{"archived":true}` {
		t.Fatalf("archive: %v (%s %s) %s", err, method, path, sent)
	}
	if err := client.SetAnchoredSessionArchived(ctx, repo, domain.ProviderClaude, "session-1", false); err != nil || sent != `{"archived":false}` {
		t.Fatalf("restore: %v %s", err, sent)
	}
	status, answer = http.StatusForbidden, `{"error":{"code":"forbidden","message":"forbidden"}}`
	if client.RequestRemoteGitRefresh(ctx, repo) == nil || client.SetAnchoredSessionArchived(ctx, repo, domain.ProviderClaude, "session-1", true) == nil {
		t.Fatal("a refusal was read as done")
	}
	if _, err := client.ListAnchoredSessions(ctx, repo); err == nil {
		t.Fatal("a refused list was read as empty")
	}
}
