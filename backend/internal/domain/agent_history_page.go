package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

const AgentHistoryPageVersion = 1
const AgentHistoryProjectionVersion = 2
const MaxAgentHistoryPageBytes = 4 << 20
const MaxAgentHistoryPageTurns = 100

var ErrContextBudgetExceeded = errors.New("context_budget_exceeded")
var ErrAgentHistoryUnavailable = errors.New("verified history read index unavailable")

type AgentHistoryPageRequest struct {
	DocIdentity       DocumentIdentity `json:"doc_identity,omitempty"`
	CoveredByIdentity DocumentIdentity `json:"covered_by_identity,omitempty"`
	Before            int              `json:"before"`
	Limit             int              `json:"limit"`
	MaxBytes          int              `json:"max_bytes"`
	CoveredBy         ContentHash      `json:"covered_by,omitempty"`
	IncompleteTail    string           `json:"incomplete_tail,omitempty"`
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

func ValidateAgentHistoryPageRequest(req AgentHistoryPageRequest) error {
	if err := req.DocIdentity.Validate(); err != nil {
		return err
	}
	if err := req.CoveredByIdentity.Validate(); err != nil {
		return err
	}
	if req.Before < -1 || req.Limit < 1 || req.Limit > MaxAgentHistoryPageTurns || req.MaxBytes < 1 || req.MaxBytes > MaxAgentHistoryPageBytes || req.CoveredBy != "" && ValidateContentHash(req.CoveredBy) != nil || req.CoveredBy == "" && req.CoveredByIdentity != DocumentIdentityLegacy || req.IncompleteTail != "" && req.IncompleteTail != "omit" {
		return fmt.Errorf("%w: invalid history page request", ErrValidation)
	}
	return nil
}
