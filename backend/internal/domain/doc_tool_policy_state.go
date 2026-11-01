package domain

import (
	"bytes"
	"encoding/json"
)

type docToolPolicyText struct {
	Token      DocFindSpan `json:"token"`
	Generation ContentHash `json:"generation,omitempty"`
	Bytes      int         `json:"bytes"`
	Prefix     []byte      `json:"prefix,omitempty"`
	Slashes    int         `json:"slashes"`
	Last       []int       `json:"last,omitempty"`
	NonSpace   bool        `json:"non_space"`
}

func (text docToolPolicyText) validate(source docToolPolicySource) error {
	if text.Token.From < 0 || text.Token.To-text.Token.From < 2 || text.Token.To > source.Event.EventBytes || text.Bytes < 0 || text.Bytes > text.Token.To-text.Token.From-2 || len(text.Prefix) != min(5, text.Bytes) || text.Slashes < 0 || text.Slashes > text.Bytes || len(text.Last) != min(3, text.Slashes) || text.Generation != "" && ValidateContentHash(text.Generation) != nil {
		return ErrIntegrity
	}
	for index, position := range text.Last {
		if position < 0 || position >= text.Bytes || index > 0 && position <= text.Last[index-1] {
			return ErrIntegrity
		}
	}
	return nil
}

func (text *docToolPolicyText) advance(page DocDisplayPage) error {
	if page.From != text.Bytes {
		return ErrIntegrity
	}
	text.Prefix = append(text.Prefix, []byte(page.Text)[:min(len(page.Text), 5-len(text.Prefix))]...)
	for offset, character := range page.Text {
		if character == '/' {
			text.Slashes++
			text.Last = append(text.Last, text.Bytes+offset)
			if len(text.Last) > 3 {
				text.Last = append([]int(nil), text.Last[1:]...)
			}
		}
		if !docRangePresentationSpace(character) {
			text.NonSpace = true
		}
	}
	text.Bytes += len(page.Text)
	return nil
}

type docToolPolicyState struct {
	Version int                 `json:"version"`
	Source  docToolPolicySource `json:"source"`
	Phase   string              `json:"phase"`
	Page    int                 `json:"page"`
	Members [7]*DocToolTreeNode `json:"members"`
	Name    *docToolPolicyText  `json:"name,omitempty"`
	Head    *docToolPolicyText  `json:"head,omitempty"`
	Stats   *docToolPolicyText  `json:"stats,omitempty"`
	Truth   [4]bool             `json:"truth"`
	TruthAt int                 `json:"truth_at"`
	Parts   []docToolEventPart  `json:"parts"`
	Part    int                 `json:"part"`
	Active  ContentHash         `json:"active,omitempty"`
	Receipt ContentHash         `json:"receipt,omitempty"`
}

func (state docToolPolicyState) validate() error {
	if state.Version != 1 || state.Source.validate() != nil || state.Page < 0 || state.Page > (state.Source.RootCount+DocToolOrderPageEntries-1)/DocToolOrderPageEntries || state.TruthAt < 0 || state.TruthAt > 4 || len(state.Parts) > docToolEventParts || state.Part < 0 || state.Part > len(state.Parts) || state.Active != "" && ValidateContentHash(state.Active) != nil || state.Receipt != "" && ValidateContentHash(state.Receipt) != nil {
		return ErrIntegrity
	}
	switch state.Phase {
	case "select", "name", "head", "truth", "layout", "parts", "complete":
	default:
		return ErrIntegrity
	}
	for _, member := range state.Members {
		if member != nil && (state.Source.Tree == nil || state.Source.Tree.Type != DocToolValueObject || member.Validate(*state.Source.Tree) != nil || member.Parent != state.Source.Tree.Root.From || member.Depth != 1 || member.Key == nil) {
			return ErrIntegrity
		}
	}
	for _, text := range []*docToolPolicyText{state.Name, state.Head, state.Stats} {
		if text != nil && text.validate(state.Source) != nil {
			return ErrIntegrity
		}
	}
	for index, part := range state.Parts {
		if part.validate(state.Source, index < state.Part || state.Phase == "complete") != nil {
			return ErrIntegrity
		}
	}
	if state.Phase == "complete" && (state.Part != len(state.Parts) || state.Active != "" || state.Stats != nil) {
		return ErrIntegrity
	}
	return nil
}

func (state docToolPolicyState) encode() ([]byte, error) {
	if state.validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func decodeDocToolPolicyState(expected ContentHash, raw []byte) (docToolPolicyState, error) {
	var state docToolPolicyState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return docToolPolicyState{}, ErrIntegrity
	}
	canonical, err := state.encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return docToolPolicyState{}, ErrIntegrity
	}
	return state, nil
}

func (state docToolPolicyState) digest() (ContentHash, error) {
	state.Receipt = ""
	raw, err := state.encode()
	return HashContent(raw), err
}

type docToolPolicyReceipt struct {
	Version int         `json:"version"`
	Profile string      `json:"profile"`
	Source  ContentHash `json:"source"`
	Digest  ContentHash `json:"digest"`
}
