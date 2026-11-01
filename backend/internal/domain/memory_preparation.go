package domain

import (
	"encoding/json"
	"errors"
)

const MemoryReadPreparationPath = "/internal/mcp/memory-preparation"
const MaxMemoryReadPreparationBytes = 16 << 10

var ErrMemoryPreparationUnavailable = errors.New("memory preparation service unavailable")

type MemoryReadPreparationRequest struct {
	Version            int                      `json:"version"`
	RepoID             ContentHash              `json:"repo_id"`
	Selection          EffectiveMemorySelection `json:"selection"`
	Content            string                   `json:"content,omitempty"`
	Cursor             string                   `json:"cursor"`
	DocumentIdentities []DocumentIdentity       `json:"document_identities"`
}

func (request MemoryReadPreparationRequest) Validate() error {
	if request.Version != 1 || request.Cursor == "" || len(request.Cursor) > MaxPreparedMemoryCursorBytes || len(request.DocumentIdentities) > 2 {
		return ErrValidation
	}
	target := MemoryReadTarget{Version: MemoryReadWorkVersion, Rules: MemoryReadRules, RepoID: request.RepoID, Selection: request.Selection, Content: request.Content}
	if err := target.Validate(); err != nil {
		return err
	}
	seen := make(map[DocumentIdentity]bool, len(request.DocumentIdentities))
	for _, identity := range request.DocumentIdentities {
		if identity.Validate() != nil || seen[identity] {
			return ErrValidation
		}
		seen[identity] = true
	}
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > MaxMemoryReadPreparationBytes {
		return ErrValidation
	}
	return nil
}
