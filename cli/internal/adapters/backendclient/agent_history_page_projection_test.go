package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestAgentHistoryClientProjectionNegotiation(t *testing.T) {
	for _, mode := range []string{"strict", "projection", "complete", "metadata", "covered", "gap only", "downgrade", "unsolicited v2", "revoked", "bad hash", "tampered events"} {
		t.Run(mode, func(t *testing.T) {
			hash, repo := domain.HashContent([]byte("source")), domain.HashContent([]byte("repo"))
			req := domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: 4 << 20, IncompleteTail: "omit"}
			if mode == "strict" || mode == "unsolicited v2" {
				req.IncompleteTail = ""
			}
			if mode == "metadata" {
				req.Before, req.Limit, req.MaxBytes = 0, 1, 1
			}
			if mode == "covered" {
				req.CoveredBy = domain.HashContent([]byte("cover"))
			}
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				q := r.URL.Query()
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer test-token" || !strings.HasSuffix(r.URL.Path, "/docs/"+string(hash)+"/turns") || q.Get("incomplete_tail") != req.IncompleteTail || q.Has("incomplete_tail") != (req.IncompleteTail != "") || q.Get("covered_by") != string(req.CoveredBy) {
					t.Error("incorrect projection request", r.URL)
				}
				if mode == "revoked" && calls == 2 {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"revoked"}}`))
					return
				}
				events := []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "prompt"}}}}
				raw, _ := json.Marshal(events)
				page := domain.AgentHistoryPage{
					Version: domain.AgentHistoryProjectionVersion, Hash: hash, Provider: domain.ProviderCodex, SessionID: "native",
					Total: 3, Before: 3, NextBefore: -1,
					Turns:       []domain.AgentHistoryTurn{{Start: 0, End: 1, Hash: domain.HashContent(raw), Events: events}},
					OmittedTail: &domain.AgentHistoryTail{Start: 1, End: 3, Reason: "incomplete_tool_pair"},
				}
				switch mode {
				case "strict", "complete", "downgrade", "unsolicited v2":
					page.Total, page.Before, page.OmittedTail = 1, 1, nil
					if mode == "strict" || mode == "downgrade" {
						page.Version = domain.AgentHistoryPageVersion
					}
				case "metadata":
					if q.Get("before") != "0" || q.Get("max_bytes") != "1" {
						t.Error("incorrect metadata query")
					}
					page.Before, page.Turns, page.OmittedTail = 0, []domain.AgentHistoryTurn{}, nil
				case "covered":
					page.Covered, page.Turns, page.OmittedTail = true, []domain.AgentHistoryTurn{}, nil
				case "gap only":
					page.Turns, page.NextBefore = []domain.AgentHistoryTurn{}, 1
				case "bad hash":
					page.Hash = repo
				case "tampered events":
					page.Turns[0].Events[0].Blocks[0].Text = "tampered"
				}
				_ = json.NewEncoder(w).Encode(page)
			}))
			defer ts.Close()
			client := NewBackendClient(func() string { return ts.URL }, func() string { return "test-token" }, domain.TeamIdentity{})
			page, err := client.ReadAgentHistoryPage(context.Background(), string(repo), hash, req)
			bad := mode == "downgrade" || mode == "unsolicited v2" || mode == "bad hash" || mode == "tampered events"
			if bad {
				if !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatalf("unsafe response accepted or wrong error: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "projection" && (page.Before != 3 || page.OmittedTail == nil || page.OmittedTail.Start != 1) {
				t.Fatal("lost source cursor or omission")
			}
			wantCalls := 1
			if mode == "revoked" {
				wantCalls = 2
				if _, err := client.ReadAgentHistoryPage(context.Background(), string(repo), hash, req); err == nil {
					t.Fatal("reused authorized projection")
				}
			}
			if calls != wantCalls {
				t.Fatalf("unexpected fallback or registration: calls=%d", calls)
			}
		})
	}
}

func TestAgentHistoryClientRejectsMalformedTailWire(t *testing.T) {
	for name, tail := range map[string]string{
		"null":              `null`,
		"array":             `[]`,
		"missing start":     `{"end":3,"reason":"incomplete_tool_pair"}`,
		"missing end":       `{"start":0,"reason":"incomplete_tool_pair"}`,
		"missing reason":    `{"start":0,"end":3}`,
		"unknown key":       `{"start":0,"end":3,"reason":"incomplete_tool_pair","extra":true}`,
		"null start":        `{"start":null,"end":3,"reason":"incomplete_tool_pair"}`,
		"null end":          `{"start":0,"end":null,"reason":"incomplete_tool_pair"}`,
		"null reason":       `{"start":0,"end":3,"reason":null}`,
		"wrong start type":  `{"start":"0","end":3,"reason":"incomplete_tool_pair"}`,
		"fractional end":    `{"start":0,"end":3.5,"reason":"incomplete_tool_pair"}`,
		"overflow end":      `{"start":0,"end":9223372036854775808,"reason":"incomplete_tool_pair"}`,
		"wrong reason type": `{"start":0,"end":3,"reason":true}`,
		"unknown reason":    `{"start":0,"end":3,"reason":"running"}`,
		"v1 tail":           `{"start":0,"end":3,"reason":"incomplete_tool_pair"}`,
	} {
		t.Run(name, func(t *testing.T) {
			hash, repo := domain.HashContent([]byte("source")), domain.HashContent([]byte("repo"))
			page := domain.AgentHistoryPage{Version: domain.AgentHistoryProjectionVersion, Hash: hash, Provider: domain.ProviderCodex, SessionID: "native", Total: 3, Before: 3, NextBefore: -1, Turns: []domain.AgentHistoryTurn{}}
			req := domain.AgentHistoryPageRequest{Before: -1, Limit: 1, MaxBytes: 1, IncompleteTail: "omit"}
			if name == "v1 tail" {
				page.Version, req.IncompleteTail = domain.AgentHistoryPageVersion, ""
			}
			raw, _ := json.Marshal(page)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			fields["omitted_tail"] = json.RawMessage(tail)
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				_ = json.NewEncoder(w).Encode(fields)
			}))
			defer ts.Close()
			client := NewBackendClient(func() string { return ts.URL }, func() string { return "test-token" }, domain.TeamIdentity{})
			if _, err := client.ReadAgentHistoryPage(context.Background(), string(repo), hash, req); err == nil {
				t.Fatal("malformed omission accepted")
			}
			if calls != 1 {
				t.Fatal("retried rejected omission", calls)
			}
		})
	}
}

func TestAgentHistoryClientUnknownTailPolicyDoesNotSend(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid policy sent") }))
	defer ts.Close()
	client := NewBackendClient(func() string { return ts.URL }, func() string { return "test-token" }, domain.TeamIdentity{})
	hash := domain.HashContent([]byte("source"))
	req := domain.AgentHistoryPageRequest{Before: -1, Limit: 1, MaxBytes: 1, IncompleteTail: "repair"}
	if _, err := client.ReadAgentHistoryPage(context.Background(), string(hash), hash, req); !errors.Is(err, domain.ErrAgentContextUnavailable) {
		t.Fatalf("invalid policy accepted: %v", err)
	}
}
