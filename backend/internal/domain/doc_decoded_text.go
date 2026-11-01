package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/json"
)

const DocDecodedTextProfile = "public-json-string-input-v1"

type DocDecodedTextSource struct {
	Version    int         `json:"version"`
	Profile    string      `json:"profile"`
	EventHash  ContentHash `json:"event_hash"`
	EventBytes int         `json:"event_bytes"`
	Token      DocFindSpan `json:"token"`
}

func (source DocDecodedTextSource) Validate() error {
	if source.Version != 1 || source.Profile != DocDecodedTextProfile || ValidateContentHash(source.EventHash) != nil || source.EventBytes <= 0 || source.EventBytes > MaxConversationDocumentBytes || source.Token.From < 0 || source.Token.From > source.EventBytes-2 || source.Token.To > source.EventBytes || source.Token.To <= source.Token.From || source.Token.To-source.Token.From < 2 {
		return ErrIntegrity
	}
	return nil
}

func (source DocDecodedTextSource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(source)
}

func (source DocDecodedTextSource) Hash() (ContentHash, error) {
	raw, err := source.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func DecodeDocDecodedTextSource(expected ContentHash, raw []byte) (DocDecodedTextSource, error) {
	var source DocDecodedTextSource
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &source) != nil {
		return DocDecodedTextSource{}, ErrIntegrity
	}
	canonical, err := source.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDecodedTextSource{}, ErrIntegrity
	}
	return source, nil
}

func (state DocFieldDiscoveryState) ReasoningTextSource() (DocDecodedTextSource, error) {
	if state.Validate() != nil || !state.Done {
		return DocDecodedTextSource{}, ErrIntegrity
	}
	if state.Kind != EventReasoning {
		return DocDecodedTextSource{}, ErrDocFindDisplayUnavailable
	}
	fields, err := state.FindFields()
	if err != nil {
		return DocDecodedTextSource{}, err
	}
	if fields.Summary == nil {
		return DocDecodedTextSource{}, ErrDocFindDisplayUnavailable
	}
	source := DocDecodedTextSource{Version: 1, Profile: DocDecodedTextProfile, EventHash: state.EventHash, EventBytes: state.EventBytes, Token: *fields.Summary}
	return source, source.Validate()
}

func encodeDocDecodedTextPage(source DocDecodedTextSource, page DocDisplayPage) ([]byte, error) {
	hash, err := source.Hash()
	if err != nil || page.validateJSONString(3, hash, source.Token) != nil {
		return nil, ErrIntegrity
	}
	raw, err := marshalDocDisplayPage(page)
	if err != nil || len(raw) > MaxDocDisplayPageBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocDecodedTextPage(source DocDecodedTextSource, expected ContentHash, raw []byte) (DocDisplayPage, error) {
	var page DocDisplayPage
	if len(raw) == 0 || len(raw) > MaxDocDisplayPageBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &page) != nil {
		return DocDisplayPage{}, ErrIntegrity
	}
	canonical, err := encodeDocDecodedTextPage(source, page)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDisplayPage{}, ErrIntegrity
	}
	return page, nil
}

type DocDecodedTextState struct {
	Version         int                          `json:"version"`
	Source          ContentHash                  `json:"source"`
	Next            int                          `json:"next"`
	TextBytes       int                          `json:"text_bytes"`
	Directory       DocRangeDirectoryAccumulator `json:"directory"`
	LookupDirectory DocRangeDirectoryAccumulator `json:"lookup_directory"`
	LookupEntries   []int                        `json:"lookup_entries,omitempty"`
	HashNext        int                          `json:"hash_next"`
	HashState       []byte                       `json:"hash_state,omitempty"`
	Hash            ContentHash                  `json:"hash,omitempty"`
	Done            bool                         `json:"done"`
}

func NewDocDecodedTextState(source DocDecodedTextSource) (DocDecodedTextState, error) {
	hash, err := source.Hash()
	if err != nil {
		return DocDecodedTextState{}, err
	}
	hashState, err := sha256.New().(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return DocDecodedTextState{}, err
	}
	return DocDecodedTextState{Version: 1, Source: hash, Next: source.Token.From, HashState: hashState}, nil
}

func (state DocDecodedTextState) Validate(source DocDecodedTextSource) error {
	hash, err := source.Hash()
	if err != nil || state.Version != 1 || state.Source != hash || state.Next < source.Token.From || state.Next > source.Token.To || state.Done != (state.Next == source.Token.To) || state.TextBytes < 0 || state.TextBytes > source.Token.To-source.Token.From-2 || state.Directory.Validate() != nil || state.LookupDirectory.Validate() != nil || (state.Directory.Count == 0) != (state.TextBytes == 0) || state.Directory.Count > state.TextBytes || len(state.LookupEntries) >= DocDecodedTextLookupEntries {
		return ErrIntegrity
	}
	if state.Directory.Count > 0 && (state.Directory.Count-1 > (state.TextBytes-1)/DocDisplayPageRuns || int64(state.TextBytes) > int64(state.Directory.Count)*DocDisplayPageTextBytes) {
		return ErrIntegrity
	}
	consumed := state.Next - source.Token.From
	if state.Next > source.Token.From {
		consumed--
	}
	if state.Done {
		consumed--
	}
	if state.TextBytes > consumed || int64(consumed) > int64(state.TextBytes)*6 || state.Directory.Count == 0 && state.Next != source.Token.From && (!state.Done || source.Token.To-source.Token.From != 2) {
		return ErrIntegrity
	}
	buckets := (state.TextBytes + DocDecodedTextLookupStride - 1) / DocDecodedTextLookupStride
	if state.Done {
		if state.LookupDirectory.Count != (buckets+DocDecodedTextLookupEntries-1)/DocDecodedTextLookupEntries || len(state.LookupEntries) != 0 || state.HashNext != state.TextBytes || len(state.HashState) != 0 || ValidateContentHash(state.Hash) != nil || state.TextBytes == 0 && state.Hash != HashContent(nil) {
			return ErrIntegrity
		}
	} else if state.LookupDirectory.Count*DocDecodedTextLookupEntries+len(state.LookupEntries) != buckets || state.HashNext != state.TextBytes/sha256.BlockSize*sha256.BlockSize || !validDocFieldHashState(state.HashState, state.HashNext) || state.Hash != "" {
		return ErrIntegrity
	}
	previous := -1
	for _, page := range state.LookupEntries {
		if page < 0 || page >= state.Directory.Count || page < previous {
			return ErrIntegrity
		}
		previous = page
	}
	if state.Next == source.Token.From {
		initial, err := NewDocDecodedTextState(source)
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

func (state DocDecodedTextState) Encode(source DocDecodedTextSource) ([]byte, error) {
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocDecodedTextState(source DocDecodedTextSource, expected ContentHash, raw []byte) (DocDecodedTextState, error) {
	var state DocDecodedTextState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocDecodedTextState{}, ErrIntegrity
	}
	canonical, err := state.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDecodedTextState{}, ErrIntegrity
	}
	return state, nil
}

type DocDecodedTextGeneration struct {
	Version   int                   `json:"version"`
	Source    DocDecodedTextSource  `json:"source"`
	Hash      ContentHash           `json:"hash"`
	TextBytes int                   `json:"text_bytes"`
	Pages     DocRangeDirectoryRoot `json:"pages"`
	Lookup    DocRangeDirectoryRoot `json:"lookup"`
}

func (generation DocDecodedTextGeneration) Validate() error {
	if generation.Version != 1 || generation.Source.Validate() != nil || ValidateContentHash(generation.Hash) != nil || generation.TextBytes < 0 || generation.TextBytes > generation.Source.Token.To-generation.Source.Token.From-2 || generation.Pages.validate(generation.Pages.Count) != nil || generation.Lookup.validate(((generation.TextBytes+DocDecodedTextLookupStride-1)/DocDecodedTextLookupStride+DocDecodedTextLookupEntries-1)/DocDecodedTextLookupEntries) != nil || (generation.Pages.Count == 0) != (generation.TextBytes == 0) || generation.Pages.Count > generation.TextBytes || generation.TextBytes == 0 && generation.Hash != HashContent(nil) {
		return ErrIntegrity
	}
	if generation.Pages.Count > 0 && (generation.Pages.Count-1 > (generation.TextBytes-1)/DocDisplayPageRuns || int64(generation.TextBytes) > int64(generation.Pages.Count)*DocDisplayPageTextBytes) {
		return ErrIntegrity
	}
	return nil
}

func (generation DocDecodedTextGeneration) Encode() ([]byte, error) {
	if generation.Validate() != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(generation)
}

func DecodeDocDecodedTextGeneration(expected ContentHash, raw []byte) (DocDecodedTextGeneration, error) {
	var generation DocDecodedTextGeneration
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &generation) != nil {
		return DocDecodedTextGeneration{}, ErrIntegrity
	}
	canonical, err := generation.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDecodedTextGeneration{}, ErrIntegrity
	}
	return generation, nil
}

func FinishDocDecodedText(ctx context.Context, source DocDecodedTextSource, state DocDecodedTextState) (result DocDecodedTextGeneration, objects []DocRangeObject, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, objects, failure = DocDecodedTextGeneration{}, nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocDecodedTextGeneration{}, nil, err
	}
	if state.Validate(source) != nil || !state.Done {
		return DocDecodedTextGeneration{}, nil, ErrIntegrity
	}
	pages, dataObjects, err := state.Directory.Finish(ctx)
	if err != nil {
		return DocDecodedTextGeneration{}, nil, err
	}
	lookup, indexObjects, err := state.LookupDirectory.Finish(ctx)
	if err != nil {
		return DocDecodedTextGeneration{}, nil, err
	}
	generation := DocDecodedTextGeneration{Version: 1, Source: source, Hash: state.Hash, TextBytes: state.TextBytes, Pages: pages, Lookup: lookup}
	if generation.Validate() != nil {
		return DocDecodedTextGeneration{}, nil, ErrIntegrity
	}
	return generation, append(dataObjects, indexObjects...), nil
}
