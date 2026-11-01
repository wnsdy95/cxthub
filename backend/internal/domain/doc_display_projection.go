package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"unicode/utf8"
)

const DocDisplayProjectionProfile = "public-event-display-v1"

type DocDisplayProjectionSource struct {
	Version    int         `json:"version"`
	Profile    string      `json:"profile"`
	EventHash  ContentHash `json:"event_hash"`
	EventBytes int         `json:"event_bytes"`
	Plan       ContentHash `json:"plan"`
	TextBytes  int         `json:"text_bytes"`
}

func (source DocDisplayProjectionSource) Validate() error {
	if source.Version != 1 || source.Profile != DocDisplayProjectionProfile || ValidateContentHash(source.EventHash) != nil || ValidateContentHash(source.Plan) != nil || source.EventBytes <= 0 || source.EventBytes > MaxConversationDocumentBytes || source.TextBytes < 0 || source.TextBytes > MaxConversationDocumentBytes {
		return ErrIntegrity
	}
	return nil
}

func (source DocDisplayProjectionSource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(source)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func (source DocDisplayProjectionSource) Hash() (ContentHash, error) {
	raw, err := source.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func DecodeDocDisplayProjectionSource(expected ContentHash, raw []byte) (DocDisplayProjectionSource, error) {
	var source DocDisplayProjectionSource
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &source) != nil {
		return DocDisplayProjectionSource{}, ErrIntegrity
	}
	canonical, err := source.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDisplayProjectionSource{}, ErrIntegrity
	}
	return source, nil
}

func (page DocDisplayPage) ValidateProjection(source DocDisplayProjectionSource) error {
	hash, err := source.Hash()
	if err != nil || page.Version != 2 || page.Source != hash || page.Index < 0 || page.Index >= MaxDocRangePages || page.From < 0 || page.From >= source.TextBytes || page.Index > page.From/DocDisplayPageRuns || int64(page.From) > int64(page.Index)*DocDisplayPageTextBytes || len(page.Text) == 0 || len(page.Text) > DocDisplayPageTextBytes || len(page.Text) > source.TextBytes-page.From {
		return ErrIntegrity
	}
	if len(page.Text) < source.TextBytes-page.From && utf8.RuneCountInString(page.Text) < DocDisplayPageRuns {
		return ErrIntegrity
	}
	return page.validateCoordinates(source.EventBytes, false)
}

func (page DocDisplayPage) EncodeProjection(source DocDisplayProjectionSource) ([]byte, error) {
	if page.ValidateProjection(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := marshalDocDisplayPage(page)
	if err != nil || len(raw) > MaxDocDisplayPageBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocDisplayProjectionPage(source DocDisplayProjectionSource, expected ContentHash, raw []byte) (DocDisplayPage, error) {
	var page DocDisplayPage
	if len(raw) == 0 || len(raw) > MaxDocDisplayPageBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &page) != nil {
		return DocDisplayPage{}, ErrIntegrity
	}
	canonical, err := page.EncodeProjection(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDisplayPage{}, ErrIntegrity
	}
	return page, nil
}

func validDocDisplayProjectionCount(count, textBytes int, done bool) bool {
	if count < 0 || count > MaxDocRangePages || textBytes < 0 || textBytes > MaxConversationDocumentBytes || (count == 0) != (textBytes == 0) {
		return false
	}
	if count == 0 {
		return true
	}
	if int64(textBytes) > int64(count)*DocDisplayPageTextBytes || count-1 > (textBytes-1)/DocDisplayPageRuns {
		return false
	}
	return done || count <= textBytes/DocDisplayPageRuns
}

type DocDisplayProjectionState struct {
	Version   int                          `json:"version"`
	Source    ContentHash                  `json:"source"`
	TextBytes int                          `json:"text_bytes"`
	Directory DocRangeDirectoryAccumulator `json:"directory"`
	Done      bool                         `json:"done"`
}

func NewDocDisplayProjectionState(source DocDisplayProjectionSource) (DocDisplayProjectionState, error) {
	hash, err := source.Hash()
	if err != nil {
		return DocDisplayProjectionState{}, err
	}
	return DocDisplayProjectionState{Version: 1, Source: hash, Done: source.TextBytes == 0}, nil
}

func (state DocDisplayProjectionState) Validate(source DocDisplayProjectionSource) error {
	if state.validateProgress(source) != nil || state.Directory.Validate() != nil {
		return ErrIntegrity
	}
	return nil
}

func (state DocDisplayProjectionState) validateProgress(source DocDisplayProjectionSource) error {
	hash, err := source.Hash()
	if err != nil || state.Version != 1 || state.Source != hash || state.TextBytes > source.TextBytes || state.Done != (state.TextBytes == source.TextBytes) || !validDocDisplayProjectionCount(state.Directory.Count, state.TextBytes, state.Done) {
		return ErrIntegrity
	}
	return nil
}

func (state DocDisplayProjectionState) Encode(source DocDisplayProjectionSource) ([]byte, error) {
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := marshalDocDisplayProjectionState(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocDisplayProjectionState(source DocDisplayProjectionSource, expected ContentHash, raw []byte) (DocDisplayProjectionState, error) {
	var state DocDisplayProjectionState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocDisplayProjectionState{}, ErrIntegrity
	}
	canonical, err := state.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDisplayProjectionState{}, ErrIntegrity
	}
	return state, nil
}

func AppendDocDisplayProjectionPage(ctx context.Context, source DocDisplayProjectionSource, state DocDisplayProjectionState, page DocDisplayPage) (result DocDisplayProjectionState, objects []DocRangeObject, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, objects, failure = DocDisplayProjectionState{}, nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocDisplayProjectionState{}, nil, err
	}
	if state.validateProgress(source) != nil || state.Done || page.Index != state.Directory.Count || page.From != state.TextBytes {
		return DocDisplayProjectionState{}, nil, ErrIntegrity
	}
	raw, err := page.EncodeProjection(source)
	if err != nil {
		return DocDisplayProjectionState{}, nil, err
	}
	hash := HashContent(raw)
	directory, produced, err := state.Directory.Append(ctx, hash)
	if err != nil {
		return DocDisplayProjectionState{}, nil, err
	}
	next := state
	next.Directory, next.TextBytes = directory, state.TextBytes+len(page.Text)
	next.Done = next.TextBytes == source.TextBytes
	if next.Validate(source) != nil {
		return DocDisplayProjectionState{}, nil, ErrIntegrity
	}
	return next, append(produced, DocRangeObject{Hash: hash, Bytes: raw}), nil
}

type DocDisplayProjectionGeneration struct {
	Version int                        `json:"version"`
	Source  DocDisplayProjectionSource `json:"source"`
	Pages   DocRangeDirectoryRoot      `json:"pages"`
}

func (generation DocDisplayProjectionGeneration) Validate() error {
	if generation.Version != 1 || generation.Source.Validate() != nil || generation.Pages.validate(generation.Pages.Count) != nil || !validDocDisplayProjectionCount(generation.Pages.Count, generation.Source.TextBytes, true) {
		return ErrIntegrity
	}
	return nil
}

func (generation DocDisplayProjectionGeneration) Encode() ([]byte, error) {
	if generation.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(generation)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocDisplayProjectionGeneration(expected ContentHash, raw []byte) (DocDisplayProjectionGeneration, error) {
	var generation DocDisplayProjectionGeneration
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &generation) != nil {
		return DocDisplayProjectionGeneration{}, ErrIntegrity
	}
	canonical, err := generation.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDisplayProjectionGeneration{}, ErrIntegrity
	}
	return generation, nil
}

func FinishDocDisplayProjection(ctx context.Context, source DocDisplayProjectionSource, state DocDisplayProjectionState) (result DocDisplayProjectionGeneration, objects []DocRangeObject, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, objects, failure = DocDisplayProjectionGeneration{}, nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocDisplayProjectionGeneration{}, nil, err
	}
	if state.validateProgress(source) != nil || !state.Done {
		return DocDisplayProjectionGeneration{}, nil, ErrIntegrity
	}
	root, objects, err := state.Directory.Finish(ctx)
	if err != nil {
		return DocDisplayProjectionGeneration{}, nil, err
	}
	generation := DocDisplayProjectionGeneration{Version: 1, Source: source, Pages: root}
	if generation.Validate() != nil {
		return DocDisplayProjectionGeneration{}, nil, ErrIntegrity
	}
	return generation, objects, nil
}

func ReadDocDisplayProjectionPage(ctx context.Context, generation DocDisplayProjectionGeneration, index int, load func(context.Context, ContentHash) ([]byte, error)) (result DocDisplayPage, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocDisplayPage{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocDisplayPage{}, err
	}
	if generation.Validate() != nil || index < 0 || index >= generation.Pages.Count || load == nil {
		return DocDisplayPage{}, ErrIntegrity
	}
	hash, err := ResolveDocRangeLeaf(ctx, generation.Pages, index, load)
	if err != nil {
		return DocDisplayPage{}, err
	}
	raw, err := load(ctx, hash)
	if err != nil {
		return DocDisplayPage{}, err
	}
	page, err := DecodeDocDisplayProjectionPage(generation.Source, hash, raw)
	if err != nil || page.Index != index || !validDocDisplayProjectionCount(generation.Pages.Count-index-1, generation.Source.TextBytes-page.From-len(page.Text), true) {
		return DocDisplayPage{}, ErrIntegrity
	}
	return page, nil
}
