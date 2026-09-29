package domain

import "errors"

const AgentHistoryPageVersion = 1
const MaxAgentHistoryPageBytes = 4 << 20
const MaxAgentHistoryPageTurns = 100

var ErrContextBudgetExceeded = errors.New("context_budget_exceeded")
var ErrAgentHistoryUnavailable = errors.New("verified history read index unavailable")

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
	// Hash covers json.Marshal(Events), not canonical document bytes. It detects
	// transport corruption; it is not an independent proof of the document hash.
	Hash   ContentHash `json:"hash"`
	Events []CIREvent  `json:"events"`
}
