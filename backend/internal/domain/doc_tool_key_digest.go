package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/json"
)

const MaxDocToolKeyDigestStateBytes = 64 << 10
const docToolKeyDigestMaxArrayIndex uint32 = 4294967294

type DocToolKeyDigestLoader func(context.Context, ContentHash) ([]byte, error)

type DocToolKeyDigestSource struct {
	Version int               `json:"version"`
	Plan    DocToolStringPlan `json:"plan"`
}

func NewDocToolKeyDigestSource(plan DocToolStringPlan) (DocToolKeyDigestSource, error) {
	source := DocToolKeyDigestSource{Version: 1, Plan: plan}
	if source.Validate() != nil {
		return DocToolKeyDigestSource{}, ErrIntegrity
	}
	return source, nil
}

func (source DocToolKeyDigestSource) Validate() error {
	if source.Version != 1 || source.Plan.Validate() != nil || source.Plan.Source.Mode != DocToolStringQuoted || source.Plan.TextBytes < 2 {
		return ErrIntegrity
	}
	return nil
}

func (source DocToolKeyDigestSource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(source)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolKeyDigestSource(expected ContentHash, raw []byte) (DocToolKeyDigestSource, error) {
	var source DocToolKeyDigestSource
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &source) != nil {
		return DocToolKeyDigestSource{}, ErrIntegrity
	}
	canonical, err := source.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolKeyDigestSource{}, ErrIntegrity
	}
	return source, nil
}

func (source DocToolKeyDigestSource) planHash() (ContentHash, error) {
	if source.Validate() != nil {
		return "", ErrIntegrity
	}
	raw, err := source.Plan.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

type DocToolKeyDigestState struct {
	Version       int         `json:"version"`
	Plan          ContentHash `json:"plan"`
	Next          int         `json:"next"`
	NextPage      int         `json:"next_page"`
	TextBytes     int         `json:"text_bytes"`
	HashNext      int         `json:"hash_next"`
	HashState     []byte      `json:"hash_state"`
	IndexPossible bool        `json:"index_possible"`
	IndexDigits   int         `json:"index_digits"`
	IndexValue    uint32      `json:"index_value"`
	Done          bool        `json:"done"`
}

func NewDocToolKeyDigestState(source DocToolKeyDigestSource) (DocToolKeyDigestState, error) {
	plan, err := source.planHash()
	if err != nil {
		return DocToolKeyDigestState{}, err
	}
	hashState, err := sha256.New().(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return DocToolKeyDigestState{}, err
	}
	return DocToolKeyDigestState{Version: 1, Plan: plan, Next: source.Plan.Source.Token.From, HashState: hashState, IndexPossible: true}, nil
}

func (state DocToolKeyDigestState) Validate(source DocToolKeyDigestSource) error {
	plan, err := source.planHash()
	if err != nil || state.Version != 1 || state.Plan != plan || state.Next < source.Plan.Source.Token.From || state.Next > source.Plan.Source.Token.To || state.NextPage < 0 || state.NextPage > source.Plan.Pages.Count || state.TextBytes < 0 || state.TextBytes > source.Plan.TextBytes || !validDocToolStringPageCount(state.NextPage, state.TextBytes, state.Done) || state.Done != (state.NextPage == source.Plan.Pages.Count) || state.Done != (state.Next == source.Plan.Source.Token.To) || state.Done != (state.TextBytes == source.Plan.TextBytes) || (state.NextPage == 0) != (state.Next == source.Plan.Source.Token.From) || source.Plan.TextBytes-state.TextBytes < source.Plan.Pages.Count-state.NextPage || int64(state.TextBytes) > int64(state.Next-source.Plan.Source.Token.From)*6+4 || state.HashNext != state.TextBytes/sha256.BlockSize*sha256.BlockSize || !validDocFieldHashState(state.HashState, state.HashNext) {
		return ErrIntegrity
	}
	contentBytes := state.TextBytes
	if state.NextPage > 0 {
		contentBytes--
	}
	if state.Done {
		contentBytes--
	}
	if state.IndexPossible {
		if state.IndexDigits < 0 || state.IndexDigits > 10 || state.IndexDigits != contentBytes || state.IndexValue > docToolKeyDigestMaxArrayIndex || state.IndexDigits == 0 && state.IndexValue != 0 || state.IndexDigits > 0 && docToolKeyDigestDigits(state.IndexValue) != state.IndexDigits {
			return ErrIntegrity
		}
	} else if contentBytes <= 0 || state.IndexDigits != 0 || state.IndexValue != 0 {
		return ErrIntegrity
	}
	if state.HashNext == 0 {
		initial, err := NewDocToolKeyDigestState(source)
		if err != nil || !bytes.Equal(initial.HashState, state.HashState) {
			return ErrIntegrity
		}
	}
	return nil
}

func (state DocToolKeyDigestState) Encode(source DocToolKeyDigestSource) ([]byte, error) {
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocToolKeyDigestStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolKeyDigestState(source DocToolKeyDigestSource, expected ContentHash, raw []byte) (DocToolKeyDigestState, error) {
	var state DocToolKeyDigestState
	if len(raw) == 0 || len(raw) > MaxDocToolKeyDigestStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocToolKeyDigestState{}, ErrIntegrity
	}
	canonical, err := state.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolKeyDigestState{}, ErrIntegrity
	}
	return state, nil
}

type DocToolKeyDigestStep struct {
	State     DocToolKeyDigestState
	TextBytes int
}

func StepDocToolKeyDigest(ctx context.Context, source DocToolKeyDigestSource, prior DocToolKeyDigestState, load DocToolKeyDigestLoader) (result DocToolKeyDigestStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolKeyDigestStep{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if prior.Validate(source) != nil || prior.Done || load == nil {
		return result, ErrIntegrity
	}
	bounded := newDocMappedObjectLoader(load)
	previous, tail, err := loadDocToolKeyDigestCheckpoint(ctx, source, prior, bounded)
	if err != nil {
		return result, err
	}
	page, err := readDocToolKeyDigestPage(ctx, source, prior.NextPage, bounded)
	if err != nil {
		return result, err
	}
	if page.RawFrom != prior.Next || page.Display.From != prior.TextBytes || previous != nil && (previous.RawTo != page.RawFrom || previous.Display.From+len(previous.Display.Text) != page.Display.From) {
		return result, ErrIntegrity
	}
	hash, err := advanceDocPreparedTextHash(docPreparedTextHashState{Next: prior.HashNext, State: prior.HashState}, prior.TextBytes, false, []byte(page.Display.Text), tail)
	if err != nil {
		return result, err
	}
	next := prior
	next.Next, next.NextPage = page.RawTo, prior.NextPage+1
	next.TextBytes += len(page.Display.Text)
	next.HashNext, next.HashState = hash.Next, hash.State
	next.Done = next.NextPage == source.Plan.Pages.Count
	from, to := 0, len(page.Display.Text)
	if page.Display.From == 0 {
		from++
	}
	if page.Display.From+to == source.Plan.TextBytes {
		to--
	}
	next.IndexPossible, next.IndexDigits, next.IndexValue = advanceDocToolKeyArrayIndex(next.IndexPossible, next.IndexDigits, next.IndexValue, page.Display.Text[from:to])
	if next.Validate(source) != nil {
		return result, ErrIntegrity
	}
	return DocToolKeyDigestStep{State: next, TextBytes: len(page.Display.Text)}, nil
}

func advanceDocToolKeyArrayIndex(possible bool, digits int, value uint32, content string) (bool, int, uint32) {
	for offset := 0; offset < len(content) && possible; offset++ {
		character := content[offset]
		if character < '0' || character > '9' || digits > 0 && value == 0 || digits == 10 || uint64(value)*10+uint64(character-'0') > uint64(docToolKeyDigestMaxArrayIndex) {
			possible, digits, value = false, 0, 0
			break
		}
		digits++
		value = value*10 + uint32(character-'0')
	}
	return possible, digits, value
}

func readDocToolKeyDigestPage(ctx context.Context, source DocToolKeyDigestSource, index int, load DocToolKeyDigestLoader) (DocToolStringPage, error) {
	page, err := ReadDocToolStringPage(ctx, source.Plan, index, load)
	if err != nil {
		return DocToolStringPage{}, err
	}
	if page.Display.From == 0 && page.Display.Text[0] != '"' || index == source.Plan.Pages.Count-1 && page.Display.Text[len(page.Display.Text)-1] != '"' || page.Display.Runs[0].SourceFrom != page.RawFrom || page.Display.Runs[len(page.Display.Runs)-1].SourceTo != page.RawTo {
		return DocToolStringPage{}, ErrIntegrity
	}
	return page, nil
}

func loadDocToolKeyDigestCheckpoint(ctx context.Context, source DocToolKeyDigestSource, state DocToolKeyDigestState, load DocToolKeyDigestLoader) (*DocToolStringPage, *DocDisplayPage, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if state.Validate(source) != nil {
		return nil, nil, ErrIntegrity
	}
	if state.NextPage == 0 {
		return nil, nil, nil
	}
	if load == nil {
		return nil, nil, ErrIntegrity
	}
	page, err := readDocToolKeyDigestPage(ctx, source, state.NextPage-1, load)
	if err != nil {
		return nil, nil, err
	}
	if page.RawTo != state.Next || page.Display.From+len(page.Display.Text) != state.TextBytes {
		return nil, nil, ErrIntegrity
	}
	tail := &page.Display
	if state.HashNext < page.Display.From {
		if state.NextPage < 2 {
			return nil, nil, ErrIntegrity
		}
		previous, err := readDocToolKeyDigestPage(ctx, source, state.NextPage-2, load)
		if err != nil {
			return nil, nil, err
		}
		if previous.RawTo != page.RawFrom || previous.Display.From+len(previous.Display.Text) != page.Display.From || state.HashNext < previous.Display.From {
			return nil, nil, ErrIntegrity
		}
		tail = &DocDisplayPage{From: state.HashNext, Text: previous.Display.Text[state.HashNext-previous.Display.From:] + page.Display.Text}
	}
	return &page, tail, nil
}

func ValidateDocToolKeyDigestCheckpoint(ctx context.Context, source DocToolKeyDigestSource, state DocToolKeyDigestState, load DocToolKeyDigestLoader) (failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
	}()
	if load != nil {
		load = newDocMappedObjectLoader(load)
	}
	_, _, err := loadDocToolKeyDigestCheckpoint(ctx, source, state, load)
	return err
}

type DocToolKeyDigestResult struct {
	Version      int         `json:"version"`
	Plan         ContentHash `json:"plan"`
	Digest       ContentHash `json:"digest"`
	TextBytes    int         `json:"text_bytes"`
	IsArrayIndex bool        `json:"is_array_index"`
	ArrayIndex   uint32      `json:"array_index"`
}

func (result DocToolKeyDigestResult) Validate(source DocToolKeyDigestSource) error {
	plan, err := source.planHash()
	if err != nil || result.Version != 1 || result.Plan != plan || ValidateContentHash(result.Digest) != nil || result.TextBytes != source.Plan.TextBytes || !result.IsArrayIndex && result.ArrayIndex != 0 || result.IsArrayIndex && (result.ArrayIndex > docToolKeyDigestMaxArrayIndex || docToolKeyDigestDigits(result.ArrayIndex) != result.TextBytes-2) {
		return ErrIntegrity
	}
	return nil
}

func (result DocToolKeyDigestResult) Encode(source DocToolKeyDigestSource) ([]byte, error) {
	if result.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(result)
}

func DecodeDocToolKeyDigestResult(source DocToolKeyDigestSource, expected ContentHash, raw []byte) (DocToolKeyDigestResult, error) {
	var result DocToolKeyDigestResult
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &result) != nil {
		return DocToolKeyDigestResult{}, ErrIntegrity
	}
	canonical, err := result.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolKeyDigestResult{}, ErrIntegrity
	}
	return result, nil
}

func FinishDocToolKeyDigest(ctx context.Context, source DocToolKeyDigestSource, state DocToolKeyDigestState, load DocToolKeyDigestLoader) (result DocToolKeyDigestResult, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolKeyDigestResult{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if state.Validate(source) != nil || !state.Done || load == nil {
		return result, ErrIntegrity
	}
	_, tail, err := loadDocToolKeyDigestCheckpoint(ctx, source, state, newDocMappedObjectLoader(load))
	if err != nil {
		return result, err
	}
	hash, err := advanceDocPreparedTextHash(docPreparedTextHashState{Next: state.HashNext, State: state.HashState}, state.TextBytes, true, nil, tail)
	if err != nil {
		return result, err
	}
	result = DocToolKeyDigestResult{Version: 1, Plan: state.Plan, Digest: hash.Hash, TextBytes: state.TextBytes, IsArrayIndex: state.IndexPossible && state.IndexDigits > 0, ArrayIndex: state.IndexValue}
	if result.Validate(source) != nil {
		return DocToolKeyDigestResult{}, ErrIntegrity
	}
	return result, nil
}

func docToolKeyDigestDigits(value uint32) int {
	digits := 1
	for value >= 10 {
		value /= 10
		digits++
	}
	return digits
}
