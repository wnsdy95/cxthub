package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/json"
)

const DocSummaryTextProfile = "public-summary-text-input-v1"
const DocSummaryTextStepSlots = 128

type DocSummaryTextSource struct {
	Version int                       `json:"version"`
	Profile string                    `json:"profile"`
	Message DocMessageInputGeneration `json:"message"`
}

func (source DocSummaryTextSource) Validate() error {
	if source.Version != 1 || source.Profile != DocSummaryTextProfile || source.Message.Validate() != nil || !source.Message.Source.Summary {
		return ErrIntegrity
	}
	return nil
}

func (source DocSummaryTextSource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(source)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func (source DocSummaryTextSource) Hash() (ContentHash, error) {
	raw, err := source.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func DecodeDocSummaryTextSource(expected ContentHash, raw []byte) (DocSummaryTextSource, error) {
	var source DocSummaryTextSource
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &source) != nil {
		return DocSummaryTextSource{}, ErrIntegrity
	}
	canonical, err := source.Encode()
	if err != nil || !bytes.Equal(raw, canonical) {
		return DocSummaryTextSource{}, ErrIntegrity
	}
	return source, nil
}

func (source DocSummaryTextSource) textLimit() int {
	if source.Message.Source.Empty {
		return 0
	}
	return source.Message.Source.Blocks.To - source.Message.Source.Blocks.From - 2
}

func encodeDocSummaryTextPage(source DocSummaryTextSource, page DocDisplayPage) ([]byte, error) {
	if validateDocSummaryTextPage(source, page) != nil {
		return nil, ErrIntegrity
	}
	raw, err := marshalDocDisplayPage(page)
	if err != nil || len(raw) > MaxDocDisplayPageBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func validateDocSummaryTextPage(source DocSummaryTextSource, page DocDisplayPage) error {
	hash, err := source.Hash()
	if err != nil || source.Message.Count == 0 || page.Version != 4 || page.Source != hash || page.Index < 0 || page.Index >= MaxDocRangePages || page.From < 0 || page.Index > page.From/DocDisplayPageRuns || int64(page.From) > int64(page.Index)*DocDisplayPageTextBytes || len(page.Text) > source.textLimit() || page.From > source.textLimit()-len(page.Text) || page.validateCoordinates(source.Message.Source.EventBytes, false) != nil {
		return ErrIntegrity
	}
	for index, run := range page.Runs {
		if run.SourceFrom <= source.Message.Source.Blocks.From || run.SourceTo >= source.Message.Source.Blocks.To || index > 0 && (run.SourceFrom < page.Runs[index-1].SourceFrom || run.SourceTo < page.Runs[index-1].SourceTo) {
			return ErrIntegrity
		}
	}
	return nil
}

func DecodeDocSummaryTextPage(source DocSummaryTextSource, expected ContentHash, raw []byte) (DocDisplayPage, error) {
	var page DocDisplayPage
	if len(raw) == 0 || len(raw) > MaxDocDisplayPageBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &page) != nil {
		return DocDisplayPage{}, ErrIntegrity
	}
	canonical, err := encodeDocSummaryTextPage(source, page)
	if err != nil || !bytes.Equal(raw, canonical) {
		return DocDisplayPage{}, ErrIntegrity
	}
	return page, nil
}

type DocSummaryTextState struct {
	Version         int                          `json:"version"`
	Source          ContentHash                  `json:"source"`
	Block           int                          `json:"block"`
	Next            int                          `json:"next"`
	Joined          bool                         `json:"joined"`
	TextBytes       int                          `json:"text_bytes"`
	PageFrom        int                          `json:"page_from"`
	Pending         ContentHash                  `json:"pending,omitempty"`
	Directory       DocRangeDirectoryAccumulator `json:"directory"`
	LookupDirectory DocRangeDirectoryAccumulator `json:"lookup_directory"`
	LookupEntries   []int                        `json:"lookup_entries,omitempty"`
	HashNext        int                          `json:"hash_next"`
	HashState       []byte                       `json:"hash_state,omitempty"`
	Hash            ContentHash                  `json:"hash,omitempty"`
	Done            bool                         `json:"done"`
}

func NewDocSummaryTextState(source DocSummaryTextSource) (DocSummaryTextState, error) {
	hash, err := source.Hash()
	if err != nil {
		return DocSummaryTextState{}, err
	}
	state := DocSummaryTextState{Version: 1, Source: hash, Done: source.Message.Count == 0}
	if state.Done {
		state.Hash = HashContent(nil)
	} else {
		state.HashState, err = sha256.New().(encoding.BinaryMarshaler).MarshalBinary()
	}
	return state, err
}

func (state DocSummaryTextState) Validate(source DocSummaryTextSource) error {
	hash, err := source.Hash()
	if err != nil || state.Version != 1 || state.Source != hash || state.Block < 0 || state.Block > source.Message.Count || state.Done != (state.Block == source.Message.Count) || state.Next < 0 || state.Next > source.Message.Source.EventBytes || state.Next > 0 && !state.Joined || state.Done && (state.Next != 0 || state.Joined) || state.TextBytes < max(0, state.Block-1) || state.TextBytes > source.textLimit() || state.PageFrom < 0 || state.PageFrom > state.TextBytes || state.TextBytes-state.PageFrom > DocDisplayPageTextBytes || state.Directory.Validate() != nil || !validDocDisplayProjectionCount(state.Directory.Count, state.PageFrom, state.Done) || state.LookupDirectory.Validate() != nil || len(state.LookupEntries) >= DocDecodedTextLookupEntries {
		return ErrIntegrity
	}
	if state.Block > 0 && state.Joined && state.TextBytes < state.Block || state.Block == 0 && state.Joined && state.Next == 0 || state.TextBytes == 0 && (state.Next != 0 || state.Joined) || state.Done && (state.PageFrom != state.TextBytes || state.Pending != "") || (state.Pending == "") != (state.PageFrom == state.TextBytes) || state.Pending != "" && ValidateContentHash(state.Pending) != nil {
		return ErrIntegrity
	}
	buckets := (state.PageFrom + DocDecodedTextLookupStride - 1) / DocDecodedTextLookupStride
	if state.Done {
		if state.LookupDirectory.Count != (buckets+DocDecodedTextLookupEntries-1)/DocDecodedTextLookupEntries || len(state.LookupEntries) != 0 || state.HashNext != state.TextBytes || len(state.HashState) != 0 || ValidateContentHash(state.Hash) != nil || state.TextBytes == 0 && state.Hash != HashContent(nil) {
			return ErrIntegrity
		}
	} else if state.LookupDirectory.Count*DocDecodedTextLookupEntries+len(state.LookupEntries) != buckets || state.HashNext != state.TextBytes/sha256.BlockSize*sha256.BlockSize || !validDocFieldHashState(state.HashState, state.HashNext) || state.Hash != "" {
		return ErrIntegrity
	}
	previous := -1
	for _, page := range state.LookupEntries {
		if page < previous || page < 0 || page >= state.Directory.Count {
			return ErrIntegrity
		}
		previous = page
	}
	if state.Block == 0 && !state.Joined && state.Next == 0 {
		initial, err := NewDocSummaryTextState(source)
		if err != nil {
			return err
		}
		actual, _ := canonicalJSON(state)
		expected, _ := canonicalJSON(initial)
		if !bytes.Equal(actual, expected) {
			return ErrIntegrity
		}
	}
	return nil
}

func (state DocSummaryTextState) Encode(source DocSummaryTextSource) ([]byte, error) {
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocSummaryTextState(source DocSummaryTextSource, expected ContentHash, raw []byte) (DocSummaryTextState, error) {
	var state DocSummaryTextState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocSummaryTextState{}, ErrIntegrity
	}
	canonical, err := state.Encode(source)
	if err != nil || !bytes.Equal(raw, canonical) {
		return DocSummaryTextState{}, ErrIntegrity
	}
	return state, nil
}

type DocSummaryTextGeneration struct {
	Version   int                   `json:"version"`
	Source    DocSummaryTextSource  `json:"source"`
	Hash      ContentHash           `json:"hash"`
	TextBytes int                   `json:"text_bytes"`
	Pages     DocRangeDirectoryRoot `json:"pages"`
	Lookup    DocRangeDirectoryRoot `json:"lookup"`
}

func (generation DocSummaryTextGeneration) Validate() error {
	if generation.Version != 1 || generation.Source.Validate() != nil || ValidateContentHash(generation.Hash) != nil || generation.TextBytes < max(0, generation.Source.Message.Count-1) || generation.Source.Message.Count == 0 && generation.TextBytes != 0 || generation.TextBytes > generation.Source.textLimit() || generation.Pages.validate(generation.Pages.Count) != nil || !validDocDisplayProjectionCount(generation.Pages.Count, generation.TextBytes, true) || generation.Lookup.validate(((generation.TextBytes+DocDecodedTextLookupStride-1)/DocDecodedTextLookupStride+DocDecodedTextLookupEntries-1)/DocDecodedTextLookupEntries) != nil || generation.TextBytes == 0 && generation.Hash != HashContent(nil) {
		return ErrIntegrity
	}
	return nil
}

func (generation DocSummaryTextGeneration) Encode() ([]byte, error) {
	if generation.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(generation)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocSummaryTextGeneration(expected ContentHash, raw []byte) (DocSummaryTextGeneration, error) {
	var generation DocSummaryTextGeneration
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &generation) != nil {
		return DocSummaryTextGeneration{}, ErrIntegrity
	}
	canonical, err := generation.Encode()
	if err != nil || !bytes.Equal(raw, canonical) {
		return DocSummaryTextGeneration{}, ErrIntegrity
	}
	return generation, nil
}
