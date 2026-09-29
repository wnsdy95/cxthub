package backendclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestPersonalWorkPrincipalRequiresFreshAuthenticatedMe(t *testing.T) {
	for _, tc := range []struct {
		name, token, body string
		status            int
		valid             bool
	}{
		{"development ID is distinct from email", "dev:alice@example.test", `{"id":"dev:alice@example.test","email":"alice@example.test"}`, 200, true},
		{"no token", "", `{"id":"dev:alice@example.test","email":"alice@example.test"}`, 200, false},
		{"no authenticated endpoint", "token", `{}`, 404, false},
		{"revoked", "token", `{}`, 401, false},
		{"no ID", "token", `{"email":"alice@example.test"}`, 200, false},
		{"no email", "token", `{"id":"alice"}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/me" || r.Header.Get("Authorization") != "Bearer "+tc.token || r.Header.Get("X-Cxt-Identity") != "" {
					t.Errorf("untrusted account request: %s, identity=%q", r.URL.Path, r.Header.Get("X-Cxt-Identity"))
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := NewBackendClient(func() string { return server.URL }, func() string { return tc.token }, domain.TeamIdentity{Email: "arbitrary-git-author@example.test"})
			for i := 0; i < 2; i++ {
				id, email, err := client.PersonalWorkPrincipal(context.Background())
				if (err == nil) != tc.valid {
					t.Fatalf("id=%q email=%q err=%v", id, email, err)
				}
				if tc.valid && (id != "dev:alice@example.test" || email != "alice@example.test") {
					t.Fatal("did not preserve backend identity")
				}
			}
			want := 2
			if tc.token == "" {
				want = 0
			}
			if calls != want {
				t.Fatalf("account reads=%d want %d", calls, want)
			}
		})
	}
}

func TestPersonalWorkDocumentBoundedBeforeDecoding(t *testing.T) {
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "session", Fidelity: domain.FidelityFull}, Events: []domain.Event{{Kind: domain.EventMessage, Role: "user", Seq: 0, Blocks: []domain.ContentBlock{{Type: "text", Text: "exact constraint"}}}}}}
	canonical, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(canonical)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	body := raw
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer server.Close()
	c := NewBackendClient(func() string { return server.URL }, func() string { return "token" }, domain.TeamIdentity{})
	repo := string(domain.HashContent([]byte("repo")))
	got, used, err := c.FetchPersonalWorkDocument(context.Background(), repo, doc.Hash, int64(len(raw)))
	if err != nil || used != int64(len(raw)) || got.Hash != doc.Hash {
		t.Fatalf("valid bounded read: used=%d err=%v", used, err)
	}
	if _, _, err := c.FetchPersonalWorkDocument(context.Background(), repo, doc.Hash, int64(len(raw)-1)); err == nil {
		t.Fatal("read exceeded remaining allowance")
	}
	body = append([]byte(" \n"), raw...)
	if _, used, err := c.FetchPersonalWorkDocument(context.Background(), repo, doc.Hash, int64(len(body))); err != nil || used != int64(len(body)) {
		t.Fatalf("wire whitespace escaped cumulative byte accounting: %d %v", used, err)
	}
	body = []byte(strings.Repeat(" ", 8<<20) + "{}")
	if _, _, err := c.FetchPersonalWorkDocument(context.Background(), repo, doc.Hash, 8<<20); err == nil {
		t.Fatal("accepted document larger than total handoff limit")
	}
	for _, remaining := range []int64{0, -1, (8 << 20) + 1} {
		if _, _, err := c.FetchPersonalWorkDocument(context.Background(), repo, doc.Hash, remaining); err == nil {
			t.Fatal("accepted invalid document budget")
		}
	}
}
