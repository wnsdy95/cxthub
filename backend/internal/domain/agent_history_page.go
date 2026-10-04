package domain

import "errors"

const AgentHistoryPageVersion = 1
const AgentHistoryProjectionVersion = 2
const MaxAgentHistoryPageBytes = 4 << 20
const MaxAgentHistoryPageTurns = 100

var ErrContextBudgetExceeded = errors.New("context_budget_exceeded")
var ErrAgentHistoryUnavailable = errors.New("verified history read index unavailable")

type AgentHistoryPageRequest struct {
	Before         int         `json:"before"`
	Limit          int         `json:"limit"`
	MaxBytes       int         `json:"max_bytes"`
	CoveredBy      ContentHash `json:"covered_by,omitempty"`
	IncompleteTail string      `json:"incomplete_tail,omitempty"`
}

type AgentHistoryPage struct {
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

// AgentHistoryTail describes an excluded whole final user turn, without changing
// the source document or its cursors. Only v2, uncovered pages at Before == Total
// may report it, with 0 <= Start < End == Total and the fixed reason below.
type AgentHistoryTail struct {
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Reason string `json:"reason"`
}

type AgentHistoryTurn struct {
	Start int `json:"start"`
	End   int `json:"end"`
	// Hash covers json.Marshal(Events), not canonical document bytes. It detects
	// transport corruption; it is not an independent proof of the document hash.
	Hash   ContentHash `json:"hash"`
	Events []CIREvent  `json:"events"`
}
