package domain

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
)

const MaxDocLegacyFindSelectorBytes = 2048

type DocLegacyFindSelector struct {
	Prepared       *DocFindPreparedProgress `json:"prepared,omitempty"`
	Representation string                   `json:"representation,omitempty"`
	Version        int                      `json:"version"`
	Repo           ContentHash              `json:"repo"`
	Hash           ContentHash              `json:"hash"`
	Base           ContentHash              `json:"base"`
	Total          int                      `json:"total"`
	Inherited      int                      `json:"inherited"`
	From           int                      `json:"from"`
	To             int                      `json:"to"`
	Index          int                      `json:"event_index"`
	Field          int                      `json:"field"`
	Byte           int                      `json:"byte_offset"`
	QueryHash      ContentHash              `json:"query_hash"`
}

func (selector DocLegacyFindSelector) Validate() error {
	if !validDocFindRepresentation(selector.Version, selector.Representation) || ValidateContentHash(selector.Repo) != nil || ValidateContentHash(selector.Hash) != nil ||
		ValidateOptionalContentHash(selector.Base) != nil || ValidateContentHash(selector.QueryHash) != nil ||
		selector.Total < 0 || selector.Total > MaxConversationDocumentBytes || selector.Inherited < 0 || selector.Inherited > selector.Total ||
		selector.From < 0 || selector.To < selector.From || selector.To > selector.Total || selector.Index < selector.From || selector.Index > selector.To ||
		selector.Field < 0 || selector.Field > 128<<10 || selector.Byte < 0 || selector.Byte > MaxConversationDocumentBytes ||
		selector.Representation == DocFindDisplayRepresentation && (selector.Field != 0 || selector.Byte > MaxDocFindDisplayBytes) ||
		!validDocFindPreparedProgress(selector.Representation, selector.Prepared, selector.Field, selector.Byte, selector.Index == selector.To) ||
		selector.Index == selector.To && (selector.Field != 0 || selector.Byte != 0) {
		return ErrValidation
	}
	return nil
}

func (selector DocLegacyFindSelector) Encode() (string, error) {
	if err := selector.Validate(); err != nil {
		return "", err
	}
	raw, err := canonicalJSON(selector)
	if err != nil || len(raw) > MaxDocLegacyFindSelectorBytes {
		return "", ErrValidation
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func DecodeDocLegacyFindSelector(encoded string) (DocLegacyFindSelector, error) {
	var selector DocLegacyFindSelector
	if len(encoded) == 0 || len(encoded) > base64.RawURLEncoding.EncodedLen(MaxDocLegacyFindSelectorBytes) {
		return selector, ErrValidation
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > MaxDocLegacyFindSelectorBytes || json.Unmarshal(raw, &selector) != nil || selector.Validate() != nil {
		return DocLegacyFindSelector{}, ErrValidation
	}
	canonical, err := canonicalJSON(selector)
	if err != nil || !bytes.Equal(canonical, raw) || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return DocLegacyFindSelector{}, ErrValidation
	}
	return selector, nil
}

func DocLegacyFindSelection(repo, hash ContentHash, request DocFindPageRequest) (DocLegacyFindSelector, error) {
	if !request.Legacy || request.Selection != "" || ValidateContentHash(repo) != nil || ValidateContentHash(hash) != nil || ValidateDocFindQuery(request.Query) != nil ||
		request.Representation != "" && request.Representation != DocFindDisplayRepresentation && request.Representation != DocFindPreparedRepresentation {
		return DocLegacyFindSelector{}, ErrValidation
	}
	queryHash := HashContent([]byte(request.Query))
	if request.Cursor != "" {
		if request.Base != "" || request.Total != 0 || request.Inherited != 0 || request.From != 0 || request.To != 0 {
			return DocLegacyFindSelector{}, ErrValidation
		}
		selector, err := DecodeDocLegacyFindSelector(request.Cursor)
		if err != nil || selector.Repo != repo || selector.Hash != hash || selector.QueryHash != queryHash || selector.Representation != request.Representation {
			return DocLegacyFindSelector{}, ErrValidation
		}
		return selector, nil
	}
	selector := DocLegacyFindSelector{Version: 1, Repo: repo, Hash: hash, Base: request.Base, Total: request.Total,
		Inherited: request.Inherited, From: request.From, To: request.To, Index: request.From, QueryHash: queryHash, Representation: request.Representation}
	if request.Representation == DocFindDisplayRepresentation {
		selector.Version = 2
	}
	if request.Representation == DocFindPreparedRepresentation {
		selector.Version, selector.Prepared = 3, &DocFindPreparedProgress{}
	}
	if err := selector.Validate(); err != nil {
		return DocLegacyFindSelector{}, err
	}
	return selector, nil
}
