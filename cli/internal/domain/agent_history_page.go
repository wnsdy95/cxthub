package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
)

const AgentHistoryPageVersion = 1

// AgentHistoryProjectionVersion is negotiated only by IncompleteTail="omit".
// The strict history-page contract remains version 1.
const AgentHistoryProjectionVersion = 2
const MaxAgentHistoryPageBytes = 4 << 20
const MaxAgentHistoryPageTurns = 100

type AgentHistoryPageRequest struct {
	DocIdentity       DocumentIdentity `json:"doc_identity,omitempty"`
	CoveredByIdentity DocumentIdentity `json:"covered_by_identity,omitempty"`
	Before            int              `json:"before"`
	Limit             int              `json:"limit"`
	MaxBytes          int              `json:"max_bytes"`
	CoveredBy         ContentHash      `json:"covered_by,omitempty"`
	// IncompleteTail may be empty (strict) or "omit" (version 2 projection).
	IncompleteTail string `json:"incomplete_tail,omitempty"`
}

type AgentHistoryPage struct {
	DocIdentity DocumentIdentity   `json:"doc_identity,omitempty"`
	Version     int                `json:"version"`
	Hash        ContentHash        `json:"hash"`
	Provider    ProviderKind       `json:"provider"`
	SessionID   string             `json:"session_id"`
	Total       int                `json:"total"`
	Before      int                `json:"before"`
	NextBefore  int                `json:"next_before"`
	Covered     bool               `json:"covered"`
	Turns       []AgentHistoryTurn `json:"turns"`
	OmittedTail *AgentHistoryTail  `json:"omitted_tail,omitempty"`
}

// AgentHistoryTail identifies a whole final user turn omitted from this source
// document. Its range is [Start, End), with End equal to the original Total.
// It records incomplete tool pairing at capture, not current session liveness.
type AgentHistoryTail struct {
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Reason string `json:"reason"`
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
	if err := req.DocIdentity.Validate(); err != nil {
		return err
	}
	if err := req.CoveredByIdentity.Validate(); err != nil {
		return err
	}
	if req.Before < -1 || req.Limit < 1 || req.Limit > MaxAgentHistoryPageTurns || req.MaxBytes < 1 || req.MaxBytes > MaxAgentHistoryPageBytes || req.CoveredBy != "" && ValidateContentHash(req.CoveredBy) != nil || req.CoveredBy == "" && req.CoveredByIdentity != DocumentIdentityLegacy || req.IncompleteTail != "" && req.IncompleteTail != "omit" {
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
	version := AgentHistoryPageVersion
	if req.IncompleteTail == "omit" {
		version = AgentHistoryProjectionVersion
	}
	if page.DocumentRef().Validate() != nil || page.DocIdentity != req.DocIdentity || ValidateContentHash(hash) != nil || page.Version != version || page.Hash != hash || page.Total < 0 || page.Before < 0 || page.Before > page.Total || page.Turns == nil || len(page.Turns) > req.Limit || (page.Provider != ProviderClaude && page.Provider != ProviderCodex) {
		return bad("identity or shape")
	}
	before := req.Before
	if before == -1 {
		before = page.Total
	}
	if page.Before != before {
		return bad("before cursor")
	}
	end := page.Before
	if tail := page.OmittedTail; tail != nil {
		if version != AgentHistoryProjectionVersion || page.Covered || page.Before != page.Total || tail.Start < 0 || tail.Start >= tail.End || tail.End != page.Total || tail.Reason != "incomplete_tool_pair" {
			return bad("omitted tail")
		}
		end = tail.Start
	}
	if page.Covered {
		if req.CoveredBy == "" || page.SessionID == "" || len(page.Turns) != 0 || page.NextBefore != -1 || page.Total > 250000 {
			return bad("coverage shape")
		}
		return nil
	}
	if len(page.Turns) == 0 {
		if page.NextBefore != -1 && (page.OmittedTail == nil || end <= 0 || page.NextBefore != end) {
			return bad("nonadvancing empty cursor")
		}
		return nil
	}
	remaining := req.MaxBytes
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

// DocumentRef identifies the source of an authorized partial projection, not
// an independently verified whole-document proof.
func (p AgentHistoryPage) DocumentRef() DocumentRef {
	return DocumentRef{Hash: p.Hash, Identity: p.DocIdentity}
}

func (req *AgentHistoryPageRequest) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ErrUnsupportedDocumentIdentity
	}
	type wire AgentHistoryPageRequest
	var next wire
	input := struct {
		*wire
		DocIdentity       singleDocumentIdentity `json:"doc_identity"`
		CoveredByIdentity singleDocumentIdentity `json:"covered_by_identity"`
	}{wire: &next}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	next.DocIdentity = input.DocIdentity.value
	next.CoveredByIdentity = input.CoveredByIdentity.value
	*req = AgentHistoryPageRequest(next)
	return nil
}
func (page *AgentHistoryPage) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ErrUnsupportedDocumentIdentity
	}
	type wire AgentHistoryPage
	var next wire
	input := struct {
		*wire
		DocIdentity singleDocumentIdentity `json:"doc_identity"`
	}{wire: &next}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	next.DocIdentity = input.DocIdentity.value
	*page = AgentHistoryPage(next)
	return nil
}
