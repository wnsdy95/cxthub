package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

type DocMessageDisplayState struct {
	Version   int                          `json:"version"`
	Source    ContentHash                  `json:"source"`
	Next      int                          `json:"next"`
	Count     int                          `json:"count"`
	TextBytes int                          `json:"text_bytes"`
	Breaks    int                          `json:"breaks"`
	Entries   []DocMessageDisplayPart      `json:"entries,omitempty"`
	Directory DocRangeDirectoryAccumulator `json:"directory"`
}

func NewDocMessageDisplayState(source DocMessageDisplaySource) (DocMessageDisplayState, error) {
	hash, err := source.Hash()
	if err != nil {
		return DocMessageDisplayState{}, err
	}
	return DocMessageDisplayState{Version: 1, Source: hash}, nil
}

func (state DocMessageDisplayState) Done(source DocMessageDisplaySource) bool {
	return state.Next == source.Input.Count
}

func (state DocMessageDisplayState) Validate(source DocMessageDisplaySource) error {
	hash, err := source.Hash()
	if err != nil || state.Version != 1 || state.Source != hash || state.Next < 0 || state.Next > source.Input.Count || state.Count < 0 || state.Count > state.Next || state.TextBytes < 0 || state.TextBytes > MaxConversationDocumentBytes || (state.Count == 0) != (state.TextBytes == 0) || int64(state.TextBytes) < int64(state.Count)*2-1 || state.Breaks < 0 || state.Breaks > 2 || state.Count > 0 && state.Breaks == 0 || state.Next == 0 && state.Breaks != 0 || len(state.Entries) >= DocMessageDisplayPageParts || state.Directory.Validate() != nil {
		return ErrIntegrity
	}
	if state.Done(source) {
		if len(state.Entries) != 0 || state.Directory.Count != (state.Count+DocMessageDisplayPageParts-1)/DocMessageDisplayPageParts {
			return ErrIntegrity
		}
	} else if state.Count != state.Directory.Count*DocMessageDisplayPageParts+len(state.Entries) {
		return ErrIntegrity
	}
	if len(state.Entries) > 0 {
		page := DocMessageDisplayPage{Version: 1, Source: hash, Index: state.Directory.Count, Parts: state.Entries}
		last := state.Entries[len(state.Entries)-1]
		if page.Validate(source) != nil || last.Index+1 != state.Count || last.Block >= state.Next || last.end() != state.TextBytes {
			return ErrIntegrity
		}
	}
	return nil
}

func (state DocMessageDisplayState) Encode(source DocMessageDisplaySource) ([]byte, error) {
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := marshalDocMessageDisplayState(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocMessageDisplayState(source DocMessageDisplaySource, expected ContentHash, raw []byte) (DocMessageDisplayState, error) {
	var state DocMessageDisplayState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocMessageDisplayState{}, ErrIntegrity
	}
	canonical, err := state.Encode(source)
	if err != nil || !bytes.Equal(raw, canonical) {
		return DocMessageDisplayState{}, ErrIntegrity
	}
	return state, nil
}

type docMessageDisplayChild struct {
	Input    DocDisplayAssemblyInput
	Kind     DocMessageDisplayKind
	Leading  int
	Trailing int
	Raw      []byte
}

func sameDocMessageDisplaySpan(left, right *DocFindSpan) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func bindDocMessageDisplayChild(input DocMessageInput, child DocMessageDisplayBlockPlan) (docMessageDisplayChild, error) {
	if input.source.Validate() != nil || input.source.Summary || input.entry.Validate(input.source) != nil {
		return docMessageDisplayChild{}, ErrIntegrity
	}
	if input.IsText() {
		source, err := input.TextSource()
		if err != nil || child.Placeholder != nil {
			return docMessageDisplayChild{}, ErrIntegrity
		}
		if source == nil {
			if child.Markdown != nil {
				return docMessageDisplayChild{}, ErrIntegrity
			}
			return docMessageDisplayChild{}, nil
		}
		if child.Markdown == nil || child.Markdown.Input.Source != *source {
			return docMessageDisplayChild{}, ErrIntegrity
		}
		raw, err := child.Markdown.Encode()
		if err != nil {
			return docMessageDisplayChild{}, err
		}
		assembly, err := NewDocDecodedMarkdownAssemblyInput(*child.Markdown)
		if err != nil {
			return docMessageDisplayChild{}, err
		}
		if assembly.source.TextBytes == 0 {
			raw = nil
		}
		return docMessageDisplayChild{Input: assembly, Kind: DocMessageDisplayMarkdown, Leading: child.Markdown.Markdown.LeadingBreaks, Trailing: child.Markdown.Markdown.TrailingBreaks, Raw: raw}, nil
	}
	if child.Markdown != nil || child.Placeholder == nil {
		return docMessageDisplayChild{}, ErrIntegrity
	}
	block := child.Placeholder.Block
	if child.Placeholder.Source != input.source || block.Index != input.entry.Index || block.Anchor != input.entry.Anchor || block.IsText != input.entry.IsText || !sameDocMessageDisplaySpan(block.Type, input.entry.Type) || !sameDocMessageDisplaySpan(block.Name, input.entry.Name) || !sameDocMessageDisplaySpan(block.Text, input.entry.Text) {
		return docMessageDisplayChild{}, ErrIntegrity
	}
	raw, err := child.Placeholder.Encode()
	if err != nil {
		return docMessageDisplayChild{}, err
	}
	assembly, err := NewDocMessagePlaceholderAssemblyInput(*child.Placeholder)
	if err != nil {
		return docMessageDisplayChild{}, err
	}
	return docMessageDisplayChild{Input: assembly, Kind: DocMessageDisplayPlaceholder, Leading: 2, Trailing: 2, Raw: raw}, nil
}

func validateDocMessageDisplayCheckpoint(ctx context.Context, source DocMessageDisplaySource, state DocMessageDisplayState, load func(context.Context, ContentHash) ([]byte, error)) (DocMessageDisplayPart, error) {
	if err := ctx.Err(); err != nil {
		return DocMessageDisplayPart{}, err
	}
	if state.Validate(source) != nil || state.Directory.Count > 0 && load == nil {
		return DocMessageDisplayPart{}, ErrIntegrity
	}
	var last DocMessageDisplayPart
	if state.Directory.Count > 0 {
		root, staged, err := state.Directory.Finish(ctx)
		if err != nil {
			return DocMessageDisplayPart{}, err
		}
		selected := func(ctx context.Context, hash ContentHash) ([]byte, error) {
			for _, object := range staged {
				if object.Hash == hash {
					return object.Bytes, nil
				}
			}
			return load(ctx, hash)
		}
		hash, err := ResolveDocRangeLeaf(ctx, root, state.Directory.Count-1, selected)
		if err != nil {
			return DocMessageDisplayPart{}, err
		}
		raw, err := selected(ctx, hash)
		if err != nil {
			return DocMessageDisplayPart{}, err
		}
		page, err := DecodeDocMessageDisplayPage(source, hash, raw)
		if err != nil || page.Index != state.Directory.Count-1 {
			return DocMessageDisplayPart{}, ErrIntegrity
		}
		expected := DocMessageDisplayPageParts
		if state.Done(source) {
			expected = state.Count - page.Index*DocMessageDisplayPageParts
		}
		last = page.Parts[len(page.Parts)-1]
		if len(page.Parts) != expected || last.Block >= state.Next {
			return DocMessageDisplayPart{}, ErrIntegrity
		}
		if len(state.Entries) > 0 {
			first := state.Entries[0]
			if first.Index != last.Index+1 || first.From != last.end() || first.Block <= last.Block || first.First.From < last.First.To {
				return DocMessageDisplayPart{}, ErrIntegrity
			}
		} else if last.Index+1 != state.Count || last.end() != state.TextBytes {
			return DocMessageDisplayPart{}, ErrIntegrity
		}
	}
	if len(state.Entries) > 0 {
		last = state.Entries[len(state.Entries)-1]
	}
	return last, ctx.Err()
}

func ValidateDocMessageDisplayCheckpoint(ctx context.Context, source DocMessageDisplaySource, state DocMessageDisplayState, load func(context.Context, ContentHash) ([]byte, error)) (failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
	}()
	if load != nil {
		load = newDocMappedObjectLoader(load)
	}
	_, err := validateDocMessageDisplayCheckpoint(ctx, source, state, load)
	return err
}

func flushDocMessageDisplayPage(ctx context.Context, source DocMessageDisplaySource, state *DocMessageDisplayState, objects *[]DocRangeObject) error {
	if len(state.Entries) == 0 {
		return ctx.Err()
	}
	page := DocMessageDisplayPage{Version: 1, Source: state.Source, Index: state.Directory.Count, Parts: state.Entries}
	raw, err := page.Encode(source)
	if err != nil {
		return err
	}
	hash := HashContent(raw)
	directory, produced, err := state.Directory.Append(ctx, hash)
	if err != nil {
		return err
	}
	*objects = append(*objects, produced...)
	*objects = append(*objects, DocRangeObject{Hash: hash, Bytes: raw})
	state.Directory, state.Entries = directory, nil
	return nil
}

func appendDocMessageDisplayChild(ctx context.Context, source DocMessageDisplaySource, state *DocMessageDisplayState, previous DocMessageDisplayPart, input DocMessageInput, child DocMessageDisplayBlockPlan, load func(context.Context, ContentHash) ([]byte, error), objects *[]DocRangeObject) (DocMessageDisplayPart, error) {
	bound, err := bindDocMessageDisplayChild(input, child)
	if err != nil {
		return DocMessageDisplayPart{}, err
	}
	if bound.Kind == "" {
		return previous, nil
	}
	breaks := max(state.Breaks, bound.Leading)
	if bound.Input.source.TextBytes == 0 {
		state.Breaks = max(breaks, bound.Trailing)
		return previous, nil
	}
	separator := 0
	if state.Count > 0 {
		separator = breaks
	}
	if bound.Input.source.TextBytes > MaxConversationDocumentBytes-state.TextBytes-separator {
		return DocMessageDisplayPart{}, ErrDocFindDisplayUnavailable
	}
	first, err := firstDocMessageDisplaySource(ctx, input, bound, load)
	if err != nil {
		return DocMessageDisplayPart{}, err
	}
	part := DocMessageDisplayPart{Index: state.Count, Block: input.Index(), From: state.TextBytes, Separator: separator, TextBytes: bound.Input.source.TextBytes, Kind: bound.Kind, Plan: HashContent(bound.Raw), First: first}
	if part.Validate(source) != nil || part.Plan != bound.Input.source.Plan || state.Count > 0 && (part.Block <= previous.Block || part.First.From < previous.First.To) {
		return DocMessageDisplayPart{}, ErrIntegrity
	}
	state.Entries = append(state.Entries, part)
	state.Count++
	state.TextBytes, state.Breaks = part.end(), bound.Trailing
	*objects = append(*objects, DocRangeObject{Hash: part.Plan, Bytes: bound.Raw})
	if len(state.Entries) == DocMessageDisplayPageParts {
		if err := flushDocMessageDisplayPage(ctx, source, state, objects); err != nil {
			return DocMessageDisplayPart{}, err
		}
	}
	return part, nil
}

type DocMessageDisplayStep struct {
	State     DocMessageDisplayState
	Objects   []DocRangeObject
	Processed int
}

func StepDocMessageDisplayPlan(ctx context.Context, source DocMessageDisplaySource, prior DocMessageDisplayState, children []DocMessageDisplayBlockPlan, load func(context.Context, ContentHash) ([]byte, error)) (result DocMessageDisplayStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocMessageDisplayStep{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocMessageDisplayStep{}, err
	}
	if _, err := prior.Encode(source); err != nil || prior.Done(source) || len(children) == 0 || len(children) > DocMessageDisplayStepBlocks || len(children) > source.Input.Count-prior.Next || load == nil {
		return DocMessageDisplayStep{}, ErrIntegrity
	}
	bounded := newDocMappedObjectLoader(load)
	previous, err := validateDocMessageDisplayCheckpoint(ctx, source, prior, bounded)
	if err != nil {
		return DocMessageDisplayStep{}, err
	}
	next := prior
	next.Entries = append([]DocMessageDisplayPart(nil), prior.Entries...)
	next.Directory = prior.Directory.clone()
	var inputs []DocMessageInput
	pageIndex := -1
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return DocMessageDisplayStep{}, err
		}
		if pageIndex != next.Next/DocMessageInputPageEntries {
			inputs, err = ReadDocMessageInputs(ctx, source.Input, next.Next/DocMessageInputPageEntries, bounded)
			if err != nil {
				if errors.Is(err, errDocMappedReplayBudget) && result.Processed > 0 {
					break
				}
				return DocMessageDisplayStep{}, err
			}
			pageIndex = next.Next / DocMessageInputPageEntries
		}
		input := inputs[next.Next%DocMessageInputPageEntries]
		if input.Index() != next.Next {
			return DocMessageDisplayStep{}, ErrIntegrity
		}
		previous, err = appendDocMessageDisplayChild(ctx, source, &next, previous, input, child, bounded, &result.Objects)
		if err != nil {
			if errors.Is(err, errDocMappedReplayBudget) && result.Processed > 0 {
				break
			}
			return DocMessageDisplayStep{}, err
		}
		next.Next++
		result.Processed++
	}
	if next.Done(source) {
		if err := flushDocMessageDisplayPage(ctx, source, &next, &result.Objects); err != nil {
			return DocMessageDisplayStep{}, err
		}
	}
	if _, err := next.Encode(source); err != nil {
		return DocMessageDisplayStep{}, err
	}
	result.State = next
	return result, nil
}

func FinishDocMessageDisplayPlan(ctx context.Context, source DocMessageDisplaySource, state DocMessageDisplayState, load func(context.Context, ContentHash) ([]byte, error)) (result DocMessageDisplayPlan, objects []DocRangeObject, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result, objects = DocMessageDisplayPlan{}, nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocMessageDisplayPlan{}, nil, err
	}
	if _, err := state.Encode(source); err != nil || !state.Done(source) {
		return DocMessageDisplayPlan{}, nil, ErrIntegrity
	}
	if load != nil {
		load = newDocMappedObjectLoader(load)
	}
	if _, err := validateDocMessageDisplayCheckpoint(ctx, source, state, load); err != nil {
		return DocMessageDisplayPlan{}, nil, err
	}
	root, objects, err := state.Directory.Finish(ctx)
	if err != nil {
		return DocMessageDisplayPlan{}, nil, err
	}
	plan := DocMessageDisplayPlan{Version: 1, Source: source, Count: state.Count, TextBytes: state.TextBytes, Parts: root}
	if _, err := plan.Encode(); err != nil {
		return DocMessageDisplayPlan{}, nil, err
	}
	return plan, objects, nil
}
