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
