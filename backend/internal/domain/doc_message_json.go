package domain

import (
	"encoding/json"
	"unicode/utf8"
)

type docMessageBlockInputJSON struct {
	Anchor DocFindSpan  `json:"anchor"`
	Index  int          `json:"index"`
	IsText bool         `json:"is_text"`
	Name   *DocFindSpan `json:"name,omitempty"`
	Text   *DocFindSpan `json:"text,omitempty"`
	Type   *DocFindSpan `json:"type,omitempty"`
}

type docMessageDisplayPartJSON struct {
	Block     int                   `json:"block"`
	First     DocFindSpan           `json:"first"`
	From      int                   `json:"from"`
	Index     int                   `json:"index"`
	Kind      DocMessageDisplayKind `json:"kind"`
	Plan      ContentHash           `json:"plan"`
	Separator int                   `json:"separator"`
	TextBytes int                   `json:"text_bytes"`
}

func marshalDocMessageInputPage(page DocMessageInputPage) ([]byte, error) {
	if !utf8.ValidString(string(page.Source)) {
		return canonicalJSON(page)
	}
	var entries []docMessageBlockInputJSON
	if page.Entries != nil {
		entries = make([]docMessageBlockInputJSON, len(page.Entries))
	}
	for index, entry := range page.Entries {
		entries[index] = docMessageBlockInputJSON{Anchor: entry.Anchor, Index: entry.Index, IsText: entry.IsText, Name: entry.Name, Text: entry.Text, Type: entry.Type}
	}
	return json.Marshal(struct {
		Entries []docMessageBlockInputJSON `json:"entries"`
		Index   int                        `json:"index"`
		Source  ContentHash                `json:"source"`
		Version int                        `json:"version"`
	}{Entries: entries, Index: page.Index, Source: page.Source, Version: page.Version})
}

func docMessageDisplayPartsJSON(parts []DocMessageDisplayPart) ([]docMessageDisplayPartJSON, bool) {
	var encoded []docMessageDisplayPartJSON
	if parts != nil {
		encoded = make([]docMessageDisplayPartJSON, len(parts))
	}
	for index, part := range parts {
		if !utf8.ValidString(string(part.Kind)) || !utf8.ValidString(string(part.Plan)) {
			return nil, false
		}
		encoded[index] = docMessageDisplayPartJSON{Block: part.Block, First: part.First, From: part.From, Index: part.Index, Kind: part.Kind, Plan: part.Plan, Separator: part.Separator, TextBytes: part.TextBytes}
	}
	return encoded, true
}

func marshalDocMessageDisplayPage(page DocMessageDisplayPage) ([]byte, error) {
	if !utf8.ValidString(string(page.Source)) {
		return canonicalJSON(page)
	}
	parts, valid := docMessageDisplayPartsJSON(page.Parts)
	if !valid {
		return canonicalJSON(page)
	}
	return json.Marshal(struct {
		Index   int                         `json:"index"`
		Parts   []docMessageDisplayPartJSON `json:"parts"`
		Source  ContentHash                 `json:"source"`
		Version int                         `json:"version"`
	}{Index: page.Index, Parts: parts, Source: page.Source, Version: page.Version})
}

func marshalDocMessageDisplayState(state DocMessageDisplayState) ([]byte, error) {
	if !utf8.ValidString(string(state.Source)) || !docDisplayDirectoryUTF8Valid(state.Directory) {
		return canonicalJSON(state)
	}
	entries, valid := docMessageDisplayPartsJSON(state.Entries)
	if !valid {
		return canonicalJSON(state)
	}
	return json.Marshal(struct {
		Breaks    int                          `json:"breaks"`
		Count     int                          `json:"count"`
		Directory DocRangeDirectoryAccumulator `json:"directory"`
		Entries   []docMessageDisplayPartJSON  `json:"entries,omitempty"`
		Next      int                          `json:"next"`
		Source    ContentHash                  `json:"source"`
		TextBytes int                          `json:"text_bytes"`
		Version   int                          `json:"version"`
	}{Breaks: state.Breaks, Count: state.Count, Directory: state.Directory, Entries: entries, Next: state.Next, Source: state.Source, TextBytes: state.TextBytes, Version: state.Version})
}
