package domain

import (
	"encoding/json"
	"testing"
)

func historyPageFixture(t *testing.T) (ContentHash, AgentHistoryPageRequest, AgentHistoryPage) {
	t.Helper()
	hash := HashContent([]byte("history source"))
	events := []Event{
		{Kind: EventMessage, Role: "user", Seq: 3, Blocks: []ContentBlock{{Type: "text", Text: "<>& \uD55C\uAD6D\uC5B4 prompt"}}},
		{Kind: EventToolCall, Seq: 4, CallID: "c", ToolName: "read"},
		{Kind: EventToolResult, Seq: 5, CallID: "c", Output: map[string]any{"value": "result"}},
		{Kind: EventMessage, Role: "assistant", Seq: 6},
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	page := AgentHistoryPage{Version: 1, Hash: hash, Provider: ProviderCodex, SessionID: "native", Total: 7, Before: 7, NextBefore: 3, Turns: []AgentHistoryTurn{{Start: 3, End: 7, Hash: HashContent(raw), Events: events}}}
	return hash, AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: len(raw)}, page
}

func TestValidateAgentHistoryPage(t *testing.T) {
	mutations := map[string]func(*AgentHistoryPage){
		"version":         func(p *AgentHistoryPage) { p.Version++ },
		"hash":            func(p *AgentHistoryPage) { p.Hash = HashContent([]byte("other")) },
		"provider":        func(p *AgentHistoryPage) { p.Provider = "other" },
		"before":          func(p *AgentHistoryPage) { p.Before-- },
		"total":           func(p *AgentHistoryPage) { p.Total-- },
		"null turns":      func(p *AgentHistoryPage) { p.Turns = nil },
		"nonadvancing":    func(p *AgentHistoryPage) { p.NextBefore = p.Before },
		"gap cursor":      func(p *AgentHistoryPage) { p.NextBefore = 1 },
		"negative cursor": func(p *AgentHistoryPage) { p.NextBefore = -2 },
		"range length":    func(p *AgentHistoryPage) { p.Turns[0].Start-- },
		"empty events":    func(p *AgentHistoryPage) { p.Turns[0].Events = nil },
		"body tamper":     func(p *AgentHistoryPage) { p.Turns[0].Events[0].Blocks[0].Text = "corrupted" },
		"no user":         func(p *AgentHistoryPage) { p.Turns[0].Events[0].Role = "assistant" },
		"split user":      func(p *AgentHistoryPage) { p.Turns[0].Events[3].Role = "user" },
		"tool result":     func(p *AgentHistoryPage) { p.Turns[0].Events[2].CallID = "other" },
		"unmatched call":  func(p *AgentHistoryPage) { p.Turns[0].Events[2] = Event{Kind: EventMessage, Role: "assistant", Seq: 5} },
		"duplicate seq":   func(p *AgentHistoryPage) { p.Turns[0].Events[2].Seq = 4 },
		"false coverage":  func(p *AgentHistoryPage) { p.Covered = true },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			hash, req, page := historyPageFixture(t)
			mutate(&page)
			if ValidateAgentHistoryPage(hash, req, page) == nil {
				t.Fatal("accepted malformed page")
			}
		})
	}
	hash, req, page := historyPageFixture(t)
	if err := ValidateAgentHistoryPage(hash, req, page); err != nil {
		t.Fatal("valid wire bound", err)
	}
	req.MaxBytes--
	if ValidateAgentHistoryPage(hash, req, page) == nil {
		t.Fatal("accepted over-byte-bound response")
	}
	req.MaxBytes++
	req.Before = 6
	if ValidateAgentHistoryPage(hash, req, page) == nil {
		t.Fatal("accepted wrong explicit cursor")
	}
	req.Before = -1
	req.CoveredBy = HashContent([]byte("cover"))
	page.Covered = true
	page.Turns = []AgentHistoryTurn{}
	page.NextBefore = -1
	if err := ValidateAgentHistoryPage(hash, req, page); err != nil {
		t.Fatal("valid covered shape", err)
	}
	page.SessionID = ""
	if ValidateAgentHistoryPage(hash, req, page) == nil {
		t.Fatal("covered missing native session")
	}
}
