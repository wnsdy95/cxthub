package domain

import (
	"encoding/base64"
	"encoding/json"
	"errors"
)

const ConversationFindPreparationPath = "/internal/mcp/conversation-find-preparation"
const MaxConversationFindPreparationBytes = 16 << 10

var ErrConversationFindPreparationRequired = errors.New("conversation find preparation is required")
var ErrConversationFindPreparationUnavailable = errors.New("conversation find preparation service unavailable")

type ConversationFindRequest struct {
	Query  string `json:"query"`
	Cursor string `json:"cursor,omitempty"`
}

func (request ConversationFindRequest) Validate() error {
	if ValidateDocFindQuery(request.Query) != nil || len(request.Cursor) > base64.RawURLEncoding.EncodedLen(MaxDocFindSelectorBytes) {
		return ErrValidation
	}
	return nil
}

type ConversationFindPage struct {
	Page         DocFindPage `json:"page"`
	ReplayCursor string      `json:"replay_cursor"`
}

type ConversationFindNotReady struct {
	Cursor string
	Cause  error
}

func (failure *ConversationFindNotReady) Error() string { return "conversation search is not ready" }
func (failure *ConversationFindNotReady) Unwrap() error { return failure.Cause }

func IsOnlyConversationFindPending(err error, readOnly bool) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		pending := false
		for _, cause := range joined.Unwrap() {
			if readOnly && cause == ErrDocFindDisplayUnavailable {
				continue
			}
			if !IsOnlyConversationFindPending(cause, readOnly) {
				return false
			}
			pending = true
		}
		return pending
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsOnlyConversationFindPending(wrapped.Unwrap(), readOnly)
	}
	return err == ErrDocRangeIndexPending || err == ErrDocFindFieldsPending || err == ErrDocDisplayPending || readOnly && (err == ErrDocRangeIndexUnavailable || err == ErrDocFindFieldsUnavailable || err == ErrConversationFindPreparationRequired)
}

type ConversationFindPreparationRequest struct {
	Version            int                     `json:"version"`
	RepoID             ContentHash             `json:"repo_id"`
	Hash               ContentHash             `json:"hash"`
	Find               ConversationFindRequest `json:"find"`
	DocumentIdentities []DocumentIdentity      `json:"document_identities"`
}

func (request ConversationFindPreparationRequest) Validate() error {
	if request.Version != 1 || ValidateContentHash(request.RepoID) != nil || ValidateContentHash(request.Hash) != nil || request.Find.Validate() != nil || len(request.DocumentIdentities) > 2 {
		return ErrValidation
	}
	seen := make(map[DocumentIdentity]bool, len(request.DocumentIdentities))
	for _, identity := range request.DocumentIdentities {
		if identity.Validate() != nil || seen[identity] {
			return ErrValidation
		}
		seen[identity] = true
	}
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > MaxConversationFindPreparationBytes {
		return ErrValidation
	}
	return nil
}
