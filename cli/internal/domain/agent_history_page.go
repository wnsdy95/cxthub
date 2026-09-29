package domain

import (
	"encoding/json"
	"fmt"
)

const AgentHistoryPageVersion = 1
const MaxAgentHistoryPageBytes = 4 << 20
const MaxAgentHistoryPageTurns = 100

type AgentHistoryPageRequest struct {
	Before    int         `json:"before"`
	Limit     int         `json:"limit"`
	MaxBytes  int         `json:"max_bytes"`
	CoveredBy ContentHash `json:"covered_by,omitempty"`
}

type AgentHistoryPage struct {
	Version    int                `json:"version"`
	Hash       ContentHash        `json:"hash"`
	Provider   ProviderKind       `json:"provider"`
	SessionID  string             `json:"session_id"`
	Total      int                `json:"total"`
	Before     int                `json:"before"`
	NextBefore int                `json:"next_before"`
	Covered    bool               `json:"covered"`
	Turns      []AgentHistoryTurn `json:"turns"`
}

type AgentHistoryTurn struct {
	Start int `json:"start"`
	End   int `json:"end"`
	// Hash verifies json.Marshal(Events). Hash on the page identifies the source
	// document; a partial response cannot independently prove its canonical hash.
	Hash   ContentHash `json:"hash"`
	Events []Event     `json:"events"`
}

func ValidateAgentHistoryPageRequest(req AgentHistoryPageRequest) error {
	if req.Before < -1 || req.Limit < 1 || req.Limit > MaxAgentHistoryPageTurns || req.MaxBytes < 1 || req.MaxBytes > MaxAgentHistoryPageBytes || req.CoveredBy != "" && ValidateContentHash(req.CoveredBy) != nil {
		return fmt.Errorf("%w: invalid history page request", ErrAgentContextUnavailable)
	}
	return nil
}

// ValidateAgentHistoryPage checks a repository-authorized server projection. It
// validates wire body hashes and contiguous descending ranges, not a full source
// hash proof. MaxBytes is a byte range cap and never an estimate of token usage.
func ValidateAgentHistoryPage(hash ContentHash, req AgentHistoryPageRequest, page AgentHistoryPage) error {
	if err := ValidateAgentHistoryPageRequest(req); err != nil {
		return err
	}
	bad := func(message string) error { return fmt.Errorf("%w: history page %s", ErrHashMismatch, message) }
	if ValidateContentHash(hash) != nil || page.Version != AgentHistoryPageVersion || page.Hash != hash || page.Total < 0 || page.Before < 0 || page.Before > page.Total || page.Turns == nil || len(page.Turns) > req.Limit || (page.Provider != ProviderClaude && page.Provider != ProviderCodex) {
		return bad("identity or shape")
	}
	before := req.Before
	if before == -1 {
		before = page.Total
	}
	if page.Before != before {
		return bad("before cursor")
	}
	if page.Covered {
		if req.CoveredBy == "" || page.SessionID == "" || len(page.Turns) != 0 || page.NextBefore != -1 || page.Total > 250000 {
			return bad("coverage shape")
		}
		return nil
	}
	if len(page.Turns) == 0 {
		if page.NextBefore != -1 {
			return bad("nonadvancing empty cursor")
		}
		return nil
	}
	remaining, end := req.MaxBytes, page.Before
	for n, turn := range page.Turns {
		if turn.Start < 0 || turn.End != end || turn.End <= turn.Start || turn.End > page.Total || turn.End-turn.Start != len(turn.Events) || ValidateContentHash(turn.Hash) != nil || turn.Events[0].Role != "user" {
			return bad("turn range")
		}
		calls := map[string]bool{}
		for i, ev := range turn.Events {
			if ev.Seq < 0 || i > 0 && ev.Seq <= turn.Events[i-1].Seq {
				return bad("event sequence")
			}
			if ev.Role == "user" && (ev.Kind != EventMessage && ev.Kind != EventTurn || i > 0 && !(i == 1 && turn.Events[0].Kind == EventTurn && ev.Kind == EventMessage)) {
				return bad("user turn boundary")
			}
			switch ev.Kind {
			case EventToolCall:
				if _, found := calls[ev.CallID]; found || ev.CallID == "" {
					return bad("tool call")
				}
				calls[ev.CallID] = false
			case EventToolResult:
				done, found := calls[ev.CallID]
				if !found || done {
					return bad("tool result")
				}
				calls[ev.CallID] = true
			}
		}
		for _, done := range calls {
			if !done {
				return bad("incomplete tool pair")
			}
		}
		if n > 0 && turn.Events[len(turn.Events)-1].Seq >= page.Turns[n-1].Events[0].Seq {
			return bad("turn sequence")
		}
		raw, err := json.Marshal(turn.Events)
		if err != nil {
			return bad("event body")
		}
		if HashContent(raw) != turn.Hash || len(raw) > remaining {
			return bad("body hash or byte bound")
		}
		remaining -= len(raw)
		end = turn.Start
	}
	if page.NextBefore != -1 && (page.NextBefore != end || end <= 0 || end >= page.Before) {
		return bad("next cursor")
	}
	return nil
}
