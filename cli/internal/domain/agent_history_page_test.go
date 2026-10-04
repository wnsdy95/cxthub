package domain

import (
	"encoding/json"
	"errors"
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

func historyProjectionFixture(t *testing.T) (ContentHash, AgentHistoryPageRequest, AgentHistoryPage) {
	t.Helper()
	hash, req, page := historyPageFixture(t)
	req.IncompleteTail = "omit"
	page.Version = AgentHistoryProjectionVersion
	page.Total, page.Before = 9, 9
	page.OmittedTail = &AgentHistoryTail{Start: 7, End: 9, Reason: "incomplete_tool_pair"}
	return hash, req, page
}

func TestValidateAgentHistoryProjection(t *testing.T) {
	hash, req, page := historyProjectionFixture(t)
	// The original document cursor is retained; only returned event bytes count
	// toward MaxBytes, which the fixture sets to exactly the returned body size.
	if err := ValidateAgentHistoryPage(hash, req, page); err != nil {
		t.Fatal(err)
	}
	req.Before = page.Total
	if err := ValidateAgentHistoryPage(hash, req, page); err != nil {
		t.Fatal("explicit EOF cursor", err)
	}
	req.MaxBytes--
	if err := ValidateAgentHistoryPage(hash, req, page); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("returned body byte bound not enforced: %v", err)
	}

	mutations := map[string]func(*AgentHistoryPageRequest, *AgentHistoryPage){
		"strict request":  func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { r.IncompleteTail = "" },
		"strict response": func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { p.Version = AgentHistoryPageVersion },
		"v1 tail": func(r *AgentHistoryPageRequest, p *AgentHistoryPage) {
			r.IncompleteTail = ""
			p.Version = AgentHistoryPageVersion
		},
		"negative start":   func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { p.OmittedTail.Start = -1 },
		"empty range":      func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { p.OmittedTail.Start = p.OmittedTail.End },
		"reversed range":   func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { p.OmittedTail.Start = p.OmittedTail.End + 1 },
		"nonterminal end":  func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { p.OmittedTail.End-- },
		"beyond total":     func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { p.OmittedTail.End++ },
		"unknown reason":   func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { p.OmittedTail.Reason = "running" },
		"empty reason":     func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { p.OmittedTail.Reason = "" },
		"later page tail":  func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { r.Before = 8; p.Before = 8 },
		"rewritten before": func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { p.Before = p.OmittedTail.Start },
		"covered tail": func(r *AgentHistoryPageRequest, p *AgentHistoryPage) {
			r.CoveredBy = HashContent([]byte("cover"))
			p.Covered = true
			p.Turns = []AgentHistoryTurn{}
			p.NextBefore = -1
		},
		"gap before returned turn": func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { p.OmittedTail.Start++ },
		"overlap returned turn":    func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { p.OmittedTail.Start-- },
		"cursor returns into tail": func(r *AgentHistoryPageRequest, p *AgentHistoryPage) { p.NextBefore = p.OmittedTail.Start },
		"returned incomplete turn": func(r *AgentHistoryPageRequest, p *AgentHistoryPage) {
			p.Turns[0].Events[2] = Event{Kind: EventMessage, Role: "assistant", Seq: 5}
			raw, _ := json.Marshal(p.Turns[0].Events)
			p.Turns[0].Hash = HashContent(raw)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			hash, req, page := historyProjectionFixture(t)
			mutate(&req, &page)
			if err := ValidateAgentHistoryPage(hash, req, page); !errors.Is(err, ErrHashMismatch) {
				t.Fatalf("malformed projection accepted or wrong error: %v", err)
			}
		})
	}
}

func TestValidateAgentHistoryProjectionEmptyPages(t *testing.T) {
	for _, tc := range []struct {
		name        string
		start, next int
		tail, valid bool
	}{
		{"gap advances", 7, 7, true, true},
		{"gap with no earlier turn", 7, -1, true, true},
		{"only omitted turn", 0, -1, true, true},
		{"zero cursor", 0, 0, true, false},
		{"gap jumps earlier", 7, 6, true, false},
		{"gap jumps inside omitted", 7, 8, true, false},
		{"gap nonadvancing", 7, 9, true, false},
		{"gap negative cursor", 7, -2, true, false},
		{"ordinary empty terminal", 0, -1, false, true},
		{"ordinary empty advancing", 0, 3, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash, req, page := historyProjectionFixture(t)
			page.Turns = []AgentHistoryTurn{}
			page.OmittedTail.Start, page.NextBefore = tc.start, tc.next
			if !tc.tail {
				page.OmittedTail = nil
			}
			if err := ValidateAgentHistoryPage(hash, req, page); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestValidateAgentHistoryProjectionNegotiation(t *testing.T) {
	for _, mode := range []string{"complete", "metadata", "covered", "continuation"} {
		t.Run(mode, func(t *testing.T) {
			hash, req, page := historyPageFixture(t)
			req.IncompleteTail, page.Version = "omit", AgentHistoryProjectionVersion
			switch mode {
			case "metadata":
				req.Before, req.MaxBytes, page.Before, page.NextBefore = 0, 1, 0, -1
				page.Turns = []AgentHistoryTurn{}
			case "covered":
				req.CoveredBy = HashContent([]byte("cover"))
				page.Covered, page.NextBefore, page.Turns = true, -1, []AgentHistoryTurn{}
			case "continuation":
				req.Before = page.Before
				page.Total += 2
			}
			if err := ValidateAgentHistoryPage(hash, req, page); err != nil {
				t.Fatal(err)
			}
			page.Version = AgentHistoryPageVersion
			if err := ValidateAgentHistoryPage(hash, req, page); !errors.Is(err, ErrHashMismatch) {
				t.Fatalf("silently downgraded opt-in: %v", err)
			}
		})
	}
	_, req, _ := historyPageFixture(t)
	for _, policy := range []string{"strict", "OMIT", " omit", "repair"} {
		req.IncompleteTail = policy
		if err := ValidateAgentHistoryPageRequest(req); !errors.Is(err, ErrAgentContextUnavailable) {
			t.Fatalf("unknown policy %q: %v", policy, err)
		}
	}
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
