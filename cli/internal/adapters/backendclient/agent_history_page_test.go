package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestAgentHistoryClientFreshAuthorizationShapeAndBodyHash(t *testing.T) {
	for _, mode := range []string{"valid", "tamper", "wrong hash", "wrong cursor", "missing field", "null field", "extra field", "covered without request", "revoked", "budget", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			hash, repo := domain.HashContent([]byte("source")), domain.HashContent([]byte("repo"))
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer test-token" || !strings.HasSuffix(r.URL.Path, "/docs/"+string(hash)+"/turns") || r.URL.Query().Get("max_bytes") != "4194304" || r.URL.Query().Get("limit") != "16" || r.URL.Query().Get("before") != "-1" {
					t.Error("bad request", r.URL)
				}
				if mode == "revoked" && calls == 2 {
					w.WriteHeader(403)
					_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"revoked"}}`)
					return
				}
				if mode == "budget" {
					w.WriteHeader(422)
					_, _ = io.WriteString(w, `{"error":{"code":"context_budget_exceeded","message":"newest turn exceeds byte range"}}`)
					return
				}
				if mode == "oversize" {
					_, _ = io.WriteString(w, strings.Repeat(" ", 32<<20)+"{}")
					return
				}
				events := []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "prompt <>&"}}}}
				raw, _ := json.Marshal(events)
				page := domain.AgentHistoryPage{Version: 1, Hash: hash, Provider: domain.ProviderCodex, SessionID: "native", Total: 1, Before: 1, NextBefore: -1, Turns: []domain.AgentHistoryTurn{{Start: 0, End: 1, Hash: domain.HashContent(raw), Events: events}}}
				switch mode {
				case "tamper":
					page.Turns[0].Events[0].Blocks[0].Text = "tampered"
				case "wrong hash":
					page.Hash = repo
				case "wrong cursor":
					page.Before = 0
				case "covered without request":
					page.Covered = true
					page.Turns = []domain.AgentHistoryTurn{}
				}
				raw, _ = json.Marshal(page)
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(raw, &fields)
				switch mode {
				case "missing field":
					delete(fields, "covered")
				case "null field":
					fields["covered"] = json.RawMessage("null")
				case "extra field":
					fields["surprise"] = json.RawMessage("false")
				}
				_ = json.NewEncoder(w).Encode(fields)
			}))
			defer ts.Close()
			c := NewBackendClient(func() string { return ts.URL }, func() string { return "test-token" }, domain.TeamIdentity{})
			req := domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: 4 << 20}
			_, err := c.ReadAgentHistoryPage(context.Background(), string(repo), hash, req)
			if mode == "valid" || mode == "revoked" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe page accepted")
			}
			if mode == "budget" && !errors.Is(err, domain.ErrContextBudgetExceeded) {
				t.Fatalf("lost budget sentinel: %v", err)
			}
			if mode == "revoked" {
				if _, err = c.ReadAgentHistoryPage(context.Background(), string(repo), hash, req); err == nil {
					t.Fatal("used stale authorized body")
				}
			}
			want := 1
			if mode == "revoked" {
				want = 2
			}
			if calls != want {
				t.Fatal("unexpected fallback or registration", calls)
			}
		})
	}
}

func TestAgentHistoryWireNormalizationCannotHideTampering(t *testing.T) {
	events := []domain.Event{{Kind: domain.EventMessage, Role: "user", Seq: 0}, {Kind: domain.EventReasoning, Seq: 1, CrossReplayable: false}}
	raw, _ := json.Marshal(events)
	page := domain.AgentHistoryPage{Version: 1, Hash: domain.HashContent([]byte("doc")), Provider: domain.ProviderCodex, SessionID: "native", Total: 2, Before: 2, NextBefore: -1, Turns: []domain.AgentHistoryTurn{{Start: 0, End: 2, Hash: domain.HashContent(raw), Events: events}}}
	wire, _ := json.Marshal(page)
	for _, changed := range []string{
		strings.Replace(string(wire), `"cross_replayable":false`, `"cross_replayable":true`, 1),
		strings.Replace(string(wire), `"cross_replayable":false`, `"cross_replayable":false,"unknown":"injected"`, 1),
		strings.Replace(string(wire), `"blocks":[]`, `"blocks":null`, 1),
	} {
		var decoded domain.AgentHistoryPage
		if err := json.Unmarshal([]byte(changed), &decoded); err != nil {
			t.Fatal(err)
		}
		// This corruption survives the typed hash because of custom marshaling.
		if err := verifyHistoryEventWire([]byte(changed), decoded); err == nil {
			t.Fatal("normalization hid wire corruption")
		}
	}
	if err := verifyHistoryEventWire(wire, page); err != nil {
		t.Fatal("legitimate wire rejected", err)
	}
}
