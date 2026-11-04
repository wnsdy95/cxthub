package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"unicode/utf8"
)

const docDisplayAssemblyRunBytes = 96

type DocDisplayAssemblyState struct {
	Version int                       `json:"version"`
	Source  ContentHash               `json:"source"`
	Output  DocDisplayProjectionState `json:"output"`
	Cursor  json.RawMessage           `json:"cursor"`
	Pending ContentHash               `json:"pending"`
}

type DocDisplayAssemblyStep struct {
	State   DocDisplayAssemblyState
	Objects []DocRangeObject
}

func NewDocDisplayAssemblyState(input DocDisplayAssemblyInput) (DocDisplayAssemblyState, error) {
	input.initial = bytes.Clone(input.initial)
	if input.validate() != nil {
		return DocDisplayAssemblyState{}, ErrIntegrity
	}
	output, err := NewDocDisplayProjectionState(input.source)
	if err != nil {
		return DocDisplayAssemblyState{}, err
	}
	state := DocDisplayAssemblyState{Version: 1, Source: output.Source, Output: output, Cursor: bytes.Clone(input.initial)}
	if _, err := state.Encode(input); err != nil {
		return DocDisplayAssemblyState{}, err
	}
	return state, nil
}

func (state DocDisplayAssemblyState) Validate(input DocDisplayAssemblyInput) error {
	input.initial = bytes.Clone(input.initial)
	if input.validate() != nil || state.Version != 1 || state.Output.Validate(input.source) != nil || state.Source != state.Output.Source || len(state.Cursor) == 0 || len(state.Cursor) > MaxDocDisplayStateBytes {
		return ErrIntegrity
	}
	canonical, err := canonicalJSON(state.Cursor)
	if err != nil || !bytes.Equal(canonical, state.Cursor) {
		return ErrIntegrity
	}
	position, err := input.cursor(bytes.Clone(state.Cursor))
	if err != nil || position < state.Output.TextBytes || position > input.source.TextBytes || position-state.Output.TextBytes > DocDisplayPageTextBytes || position == 0 && !bytes.Equal(state.Cursor, input.initial) {
		return ErrIntegrity
	}
	if (state.Pending == "") != (position == state.Output.TextBytes) || state.Pending != "" && ValidateContentHash(state.Pending) != nil || state.Output.Done && state.Pending != "" {
		return ErrIntegrity
	}
	return nil
}

func (state DocDisplayAssemblyState) Encode(input DocDisplayAssemblyInput) ([]byte, error) {
	if state.Validate(input) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocDisplayAssemblyState(input DocDisplayAssemblyInput, expected ContentHash, raw []byte) (DocDisplayAssemblyState, error) {
	var state DocDisplayAssemblyState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocDisplayAssemblyState{}, ErrIntegrity
	}
	canonical, err := state.Encode(input)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDisplayAssemblyState{}, ErrIntegrity
	}
	return state, nil
}

func encodeDocDisplayAssemblyPending(source DocDisplayProjectionSource, page DocDisplayPage) ([]byte, error) {
	hash, err := source.Hash()
	if err != nil || page.Version != 2 || page.Source != hash || page.Index < 0 || page.Index >= MaxDocRangePages || page.From < 0 || page.From >= source.TextBytes || page.Index > page.From/DocDisplayPageRuns || int64(page.From) > int64(page.Index)*DocDisplayPageTextBytes || len(page.Text) > source.TextBytes-page.From || page.validateCoordinates(source.EventBytes, false) != nil {
		return nil, ErrIntegrity
	}
	raw, err := marshalDocDisplayPage(page)
	if err != nil || len(raw) > MaxDocDisplayPageBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func decodeDocDisplayAssemblyPending(source DocDisplayProjectionSource, expected ContentHash, raw []byte) (DocDisplayPage, error) {
	var page DocDisplayPage
	if len(raw) == 0 || len(raw) > MaxDocDisplayPageBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &page) != nil {
		return DocDisplayPage{}, ErrIntegrity
	}
	canonical, err := encodeDocDisplayAssemblyPending(source, page)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDisplayPage{}, ErrIntegrity
	}
	return page, nil
}

func validateDocDisplayAssemblyCheckpoint(ctx context.Context, input DocDisplayAssemblyInput, state DocDisplayAssemblyState, load func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, error) {
	if err := ctx.Err(); err != nil {
		return DocDisplayPage{}, err
	}
	if state.Validate(input) != nil || (state.Pending != "" || state.Output.Directory.Count > 0) && load == nil {
		return DocDisplayPage{}, ErrIntegrity
	}
	var pending DocDisplayPage
	if state.Pending != "" {
		raw, err := load(ctx, state.Pending)
		if err != nil {
			return DocDisplayPage{}, err
		}
		pending, err = decodeDocDisplayAssemblyPending(input.source, state.Pending, raw)
		if err != nil {
			return DocDisplayPage{}, err
		}
		position, err := input.cursor(bytes.Clone(state.Cursor))
		if err != nil || pending.Index != state.Output.Directory.Count || pending.From != state.Output.TextBytes || len(pending.Text) != position-state.Output.TextBytes {
			return DocDisplayPage{}, ErrIntegrity
		}
	}
	if state.Output.Directory.Count > 0 {
		root, staged, err := state.Output.Directory.Finish(ctx)
		if err != nil {
			return DocDisplayPage{}, err
		}
		selected := func(ctx context.Context, hash ContentHash) ([]byte, error) {
			for _, object := range staged {
				if object.Hash == hash {
					return object.Bytes, nil
				}
			}
			return load(ctx, hash)
		}
		hash, err := ResolveDocRangeLeaf(ctx, root, state.Output.Directory.Count-1, selected)
		if err != nil {
			return DocDisplayPage{}, err
		}
		raw, err := selected(ctx, hash)
		if err != nil {
			return DocDisplayPage{}, err
		}
		last, err := DecodeDocDisplayProjectionPage(input.source, hash, raw)
		if err != nil || last.Index != state.Output.Directory.Count-1 || last.From+len(last.Text) != state.Output.TextBytes || !validDocDisplayProjectionCount(last.Index, last.From, false) {
			return DocDisplayPage{}, ErrIntegrity
		}
	}
	return pending, ctx.Err()
}

func ValidateDocDisplayAssemblyCheckpoint(ctx context.Context, input DocDisplayAssemblyInput, state DocDisplayAssemblyState, load func(context.Context, ContentHash) ([]byte, error)) (failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
	}()
	_, err := validateDocDisplayAssemblyCheckpoint(ctx, input, state, load)
	return err
}

func docDisplayAssemblyCanSeal(source DocDisplayProjectionSource, page DocDisplayPage) bool {
	return len(page.Text) > 0 && (page.From+len(page.Text) == source.TextBytes || utf8.RuneCountInString(page.Text) >= DocDisplayPageRuns)
}

func docDisplayAssemblyReadBudget(page DocDisplayPage) (int, int, error) {
	raw, err := marshalDocDisplayPage(page)
	if err != nil || len(raw) >= MaxDocDisplayPageBytes {
		return 0, 0, ErrIntegrity
	}
	maxRuns := DocDisplayPageRuns - len(page.Runs)
	maxBytes := min(DocDisplayPageTextBytes-len(page.Text), (MaxDocDisplayPageBytes-len(raw)-maxRuns*docDisplayAssemblyRunBytes)/6)
	if maxRuns <= 0 || maxBytes < utf8.UTFMax {
		return 0, 0, ErrIntegrity
	}
	return maxBytes, maxRuns, nil
}

func StepDocDisplayAssembly(ctx context.Context, input DocDisplayAssemblyInput, prior DocDisplayAssemblyState, load func(context.Context, ContentHash) ([]byte, error)) (result DocDisplayAssemblyStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocDisplayAssemblyStep{}
		}
	}()
	pending, err := validateDocDisplayAssemblyCheckpoint(ctx, input, prior, load)
	if err != nil {
		return DocDisplayAssemblyStep{}, err
	}
	if prior.Output.Done {
		return DocDisplayAssemblyStep{}, ErrIntegrity
	}
	next := prior
	next.Cursor = bytes.Clone(prior.Cursor)
	if pending.Text == "" {
		pending = DocDisplayPage{Version: 2, Source: prior.Source, Index: prior.Output.Directory.Count, From: prior.Output.TextBytes}
	}
	if !docDisplayAssemblyCanSeal(input.source, pending) {
		maxBytes, maxRuns, err := docDisplayAssemblyReadBudget(pending)
		if err != nil {
			return DocDisplayAssemblyStep{}, err
		}
		maxBytes = min(maxBytes, input.source.TextBytes-prior.Output.TextBytes-len(pending.Text))
		fragment, cursor, err := input.read(ctx, bytes.Clone(prior.Cursor), maxBytes, maxRuns, load)
		if err != nil {
			return DocDisplayAssemblyStep{}, err
		}
		position, err := input.cursor(bytes.Clone(cursor))
		if err != nil || fragment.Version != 2 || fragment.Source != prior.Source || fragment.From != prior.Output.TextBytes+len(pending.Text) || len(fragment.Text) > maxBytes || len(fragment.Runs) > maxRuns || position != fragment.From+len(fragment.Text) || bytes.Equal(cursor, prior.Cursor) {
			return DocDisplayAssemblyStep{}, ErrIntegrity
		}
		if fragment.validateCoordinates(input.source.EventBytes, false) != nil {
			return DocDisplayAssemblyStep{}, ErrIntegrity
		}
		offset := len(pending.Text)
		pending.Text += fragment.Text
		for _, run := range fragment.Runs {
			run.To += offset
			if len(pending.Runs) > 0 && run.Linear && pending.Runs[len(pending.Runs)-1].Linear && pending.Runs[len(pending.Runs)-1].SourceTo == run.SourceFrom {
				last := &pending.Runs[len(pending.Runs)-1]
				last.To, last.SourceTo = run.To, run.SourceTo
			} else {
				pending.Runs = append(pending.Runs, run)
			}
		}
		next.Cursor = bytes.Clone(cursor)
	}
	if docDisplayAssemblyCanSeal(input.source, pending) {
		if _, err := encodeDocDisplayAssemblyPending(input.source, pending); err != nil {
			return DocDisplayAssemblyStep{}, err
		}
		next.Output, result.Objects, err = AppendDocDisplayProjectionPage(ctx, input.source, prior.Output, pending)
		if err != nil {
			return DocDisplayAssemblyStep{}, err
		}
		next.Pending = ""
	} else if pending.Text != "" {
		raw, err := encodeDocDisplayAssemblyPending(input.source, pending)
		if err != nil {
			return DocDisplayAssemblyStep{}, err
		}
		next.Pending = HashContent(raw)
		if next.Pending != prior.Pending {
			result.Objects = []DocRangeObject{{Hash: next.Pending, Bytes: raw}}
		}
	}
	if _, err := next.Encode(input); err != nil {
		return DocDisplayAssemblyStep{}, err
	}
	result.State = next
	return result, nil
}

func FinishDocDisplayAssembly(ctx context.Context, input DocDisplayAssemblyInput, state DocDisplayAssemblyState, load func(context.Context, ContentHash) ([]byte, error)) (result DocDisplayProjectionGeneration, objects []DocRangeObject, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result, objects = DocDisplayProjectionGeneration{}, nil
		}
	}()
	if err := ValidateDocDisplayAssemblyCheckpoint(ctx, input, state, load); err != nil {
		return DocDisplayProjectionGeneration{}, nil, err
	}
	return FinishDocDisplayProjection(ctx, input.source, state.Output)
}
