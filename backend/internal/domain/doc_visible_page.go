package domain

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
)

const DocVisibleProjection = "visible-v1"
const MaxDocVisibleSelectorBytes = 2048

var ErrDocVisibleProjectionUnsupported = errors.New("visible document projection unsupported")
var ErrDocVisibleGenerationStale = errors.New("visible document generation changed")

type DocVisiblePageRequest struct {
	Base   ContentHash
	View   string
	Offset int
	Limit  int
	Cursor string
	Expand string
}

type DocVisibleEvent struct {
	DocViewEvent
	SeedSummary    bool
	EmptyReasoning bool
}

func (event DocVisibleEvent) MarshalJSON() ([]byte, error) {
	raw, err := event.DocViewEvent.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields["seed_summary"], err = json.Marshal(event.SeedSummary)
	if err != nil {
		return nil, err
	}
	fields["empty_reasoning"], err = json.Marshal(event.EmptyReasoning)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

type DocVisibleFold struct {
	From   int    `json:"from"`
	To     int    `json:"to"`
	Rows   int    `json:"rows"`
	Cursor string `json:"cursor"`
}

type DocVisiblePageUnit struct {
	From      int              `json:"from"`
	To        int              `json:"to"`
	TurnIndex int              `json:"turn_index"`
	Event     *DocVisibleEvent `json:"event"`
	Fold      *DocVisibleFold  `json:"fold"`
}

type DocVisibleFoldRange struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type DocVisiblePage struct {
	Projection   string               `json:"projection"`
	Kind         string               `json:"kind"`
	Hash         ContentHash          `json:"hash"`
	Base         ContentHash          `json:"base"`
	Generation   ContentHash          `json:"generation"`
	Rules        int                  `json:"rules"`
	View         string               `json:"view"`
	Envelope     CIREnvelope          `json:"envelope"`
	Total        int                  `json:"total"`
	Inherited    int                  `json:"inherited"`
	Start        int                  `json:"start"`
	From         int                  `json:"from"`
	To           int                  `json:"to"`
	Units        []DocVisiblePageUnit `json:"units"`
	Fold         *DocVisibleFoldRange `json:"fold"`
	Events       []DocVisibleEvent    `json:"events"`
	ReplayCursor string               `json:"replay_cursor"`
	NextCursor   *string              `json:"next_cursor"`
}

type DocVisibleSelector struct {
	Version    int         `json:"version"`
	Purpose    string      `json:"purpose"`
	Repo       ContentHash `json:"repo"`
	Hash       ContentHash `json:"hash"`
	Base       ContentHash `json:"base"`
	Generation ContentHash `json:"generation"`
	View       string      `json:"view"`
	Start      int         `json:"start"`
	Total      int         `json:"total"`
	Inherited  int         `json:"inherited"`
	Unit       int         `json:"unit"`
	Next       int         `json:"next"`
	Limit      int         `json:"limit"`
}

func (selector DocVisibleSelector) Validate() error {
	terminal := selector.Purpose == "units" && selector.Start == selector.Total && selector.Next == selector.Total && selector.Unit == 0
	if selector.Version != 1 || selector.Purpose != "units" && selector.Purpose != "fold" ||
		ValidateContentHash(selector.Repo) != nil || ValidateContentHash(selector.Hash) != nil ||
		ValidateOptionalContentHash(selector.Base) != nil || ValidateContentHash(selector.Generation) != nil ||
		selector.View != "all" && selector.View != "chat" && selector.View != "prompts" ||
		selector.Total < 0 || selector.Total > MaxConversationDocumentBytes ||
		selector.Start < 0 || selector.Inherited < 0 || selector.Inherited > selector.Total ||
		!terminal && (selector.Start >= selector.Total || selector.Unit < 0 || selector.Unit >= selector.Total || selector.Next < selector.Start || selector.Next >= selector.Total) ||
		selector.Limit <= 0 || selector.Limit > 100 || selector.Base == "" && selector.Inherited != 0 {
		return ErrValidation
	}
	return nil
}

func (selector DocVisibleSelector) Encode() (string, error) {
	if err := selector.Validate(); err != nil {
		return "", err
	}
	raw, err := canonicalJSON(selector)
	if err != nil || len(raw) > MaxDocVisibleSelectorBytes {
		return "", ErrValidation
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func DecodeDocVisibleSelector(encoded string) (DocVisibleSelector, error) {
	var selector DocVisibleSelector
	if len(encoded) == 0 || len(encoded) > base64.RawURLEncoding.EncodedLen(MaxDocVisibleSelectorBytes) {
		return selector, ErrValidation
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > MaxDocVisibleSelectorBytes || json.Unmarshal(raw, &selector) != nil || selector.Validate() != nil {
		return DocVisibleSelector{}, ErrValidation
	}
	canonical, err := canonicalJSON(selector)
	if err != nil || !bytes.Equal(raw, canonical) || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return DocVisibleSelector{}, ErrValidation
	}
	return selector, nil
}
