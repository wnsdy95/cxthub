package memoryquery

import (
	"encoding/base64"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const MaxPageBytes = 256 << 10
const MaxItems = 16384

type Cursor struct {
	Version int                `json:"v"`
	State   domain.ContentHash `json:"state"`
	Index   int                `json:"index"`
}

func NormalizeRequest(repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryRequest, error) {
	if request.Content == "all" {
		request.Content = ""
	}
	if request.Content != "" && request.Content != "claims" && request.Content != "prompt" {
		return request, domain.ErrValidation
	}
	if domain.ValidateContentHash(repo) != nil || request.Selection.Validate() != nil || request.Limit < 0 || request.Limit > 50 || len(request.Cursor) > 1024 {
		return request, domain.ErrValidation
	}
	if request.Limit == 0 {
		request.Limit = 20
	}
	return request, nil
}

func ReadTarget(repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadTarget, error) {
	normalized, err := NormalizeRequest(repo, request)
	if err != nil {
		return domain.MemoryReadTarget{}, err
	}
	target := domain.MemoryReadTarget{Version: domain.MemoryReadWorkVersion, Rules: domain.MemoryReadRules,
		RepoID: repo, Selection: normalized.Selection, Content: normalized.Content}
	return target, target.Validate()
}

func Offset(encoded string, state domain.ContentHash) (int, error) {
	if encoded == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, domain.ErrValidation
	}
	var cursor Cursor
	if json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Index < 0 {
		return 0, domain.ErrValidation
	}
	if cursor.State != state {
		return 0, domain.ErrEffectiveMemoryCursorStale
	}
	return cursor.Index, nil
}
