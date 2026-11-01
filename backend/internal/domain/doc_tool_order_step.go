package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

type DocToolOrderState struct {
	Version   int                          `json:"version"`
	Source    ContentHash                  `json:"source"`
	Next      int                          `json:"next"`
	After     *DocToolOrderEntry           `json:"after,omitempty"`
	Directory DocRangeDirectoryAccumulator `json:"directory"`
}

func NewDocToolOrderState(tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrderSource) (DocToolOrderState, error) {
	hash, err := source.Hash(tree, keys)
	if err != nil {
		return DocToolOrderState{}, err
	}
	return DocToolOrderState{Version: 1, Source: hash}, nil
}

func (state DocToolOrderState) Done(source DocToolOrderSource) bool {
	return state.Next == source.Count
}

func (state DocToolOrderState) Validate(tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrderSource) error {
	hash, err := source.Hash(tree, keys)
	if err != nil || state.Version != 1 || state.Source != hash || state.Next < 0 || state.Next > source.Count || state.Directory.Validate() != nil || state.Directory.Count != (state.Next+DocToolOrderPageEntries-1)/DocToolOrderPageEntries || state.Next < source.Count && state.Next%DocToolOrderPageEntries != 0 || (state.Next == 0) != (state.After == nil) {
		return ErrIntegrity
	}
	if state.After != nil && (state.After.Validate(tree, keys, source) != nil || source.Parent.Type == DocToolValueArray && state.After.Node.Ordinal != state.Next-1) {
		return ErrIntegrity
	}
	return nil
}

func (state DocToolOrderState) Encode(tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrderSource) ([]byte, error) {
	if state.Validate(tree, keys, source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolOrderState(tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrderSource, expected ContentHash, raw []byte) (DocToolOrderState, error) {
	var state DocToolOrderState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocToolOrderState{}, ErrIntegrity
	}
	canonical, err := state.Encode(tree, keys, source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolOrderState{}, ErrIntegrity
	}
	return state, nil
}

func validateDocToolOrderCheckpoint(ctx context.Context, tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrderSource, state DocToolOrderState, load func(context.Context, ContentHash) ([]byte, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.Validate(tree, keys, source) != nil {
		return ErrIntegrity
	}
	if state.Next == 0 {
		return nil
	}
	if load == nil {
		return ErrIntegrity
	}
	root, objects, err := state.Directory.Finish(ctx)
	if err != nil {
		return err
	}
	selected := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		for _, object := range objects {
			if object.Hash == hash {
				return bytes.Clone(object.Bytes), nil
			}
		}
		return load(ctx, hash)
	}
	hash, err := ResolveDocRangeLeaf(ctx, root, root.Count-1, selected)
	if err != nil {
		return err
	}
	raw, err := selected(ctx, hash)
	if err != nil {
		return err
	}
	page, err := DecodeDocToolOrderPage(tree, keys, source, hash, raw)
	if err != nil || page.Index != root.Count-1 || page.Index*DocToolOrderPageEntries+len(page.Entries) != state.Next {
		return ErrIntegrity
	}
	last, _ := canonicalJSON(page.Entries[len(page.Entries)-1])
	after, _ := canonicalJSON(state.After)
	if !bytes.Equal(last, after) {
		return ErrIntegrity
	}
	return ctx.Err()
}

type DocToolOrderEntryLoader func(context.Context, *DocToolOrderEntry, int) ([]DocToolOrderEntry, error)

type DocToolOrderStep struct {
	source  DocToolOrderSource
	prior   ContentHash
	state   DocToolOrderState
	entries []DocToolOrderEntry
	objects []DocRangeObject
}

func (step DocToolOrderStep) Source() DocToolOrderSource {
	source := step.source
	source.Parent = cloneDocToolTreeNode(source.Parent)
	return source
}

func (step DocToolOrderStep) Prior() ContentHash { return step.prior }

func (step DocToolOrderStep) State() DocToolOrderState {
	state := step.state
	state.Directory = state.Directory.clone()
	if state.After != nil {
		after := cloneDocToolOrderEntry(*state.After)
		state.After = &after
	}
	return state
}

func (step DocToolOrderStep) Entries() []DocToolOrderEntry {
	result := make([]DocToolOrderEntry, len(step.entries))
	for index, entry := range step.entries {
		result[index] = cloneDocToolOrderEntry(entry)
	}
	return result
}

func (step DocToolOrderStep) Objects() []DocRangeObject {
	result := make([]DocRangeObject, len(step.objects))
	for index, object := range step.objects {
		result[index] = DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)}
	}
	return result
}

func StepDocToolOrder(ctx context.Context, tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrderSource, prior DocToolOrderState, loadEntries DocToolOrderEntryLoader, loadObject func(context.Context, ContentHash) ([]byte, error)) (result DocToolOrderStep, failure error) {
	source.Parent = cloneDocToolTreeNode(source.Parent)
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolOrderStep{}
		}
	}()
	if err := validateDocToolOrderCheckpoint(ctx, tree, keys, source, prior, loadObject); err != nil {
		return result, err
	}
	if prior.Done(source) || loadEntries == nil {
		return result, ErrIntegrity
	}
	encoded, err := prior.Encode(tree, keys, source)
	if err != nil {
		return result, err
	}
	state, err := DecodeDocToolOrderState(tree, keys, source, HashContent(encoded), encoded)
	if err != nil {
		return result, err
	}
	count := min(DocToolOrderPageEntries, source.Count-prior.Next)
	var cursor *DocToolOrderEntry
	if state.After != nil {
		after := cloneDocToolOrderEntry(*state.After)
		cursor = &after
	}
	entries, err := loadEntries(ctx, cursor, count)
	if err != nil {
		return result, err
	}
	if len(entries) != count {
		return result, ErrIntegrity
	}
	page := DocToolOrderPage{Version: 1, Source: state.Source, Index: state.Directory.Count, Entries: make([]DocToolOrderEntry, len(entries))}
	for index, entry := range entries {
		page.Entries[index] = cloneDocToolOrderEntry(entry)
	}
	if prior.After != nil && CompareDocToolOrderEntries(*prior.After, page.Entries[0], source) >= 0 {
		return result, ErrIntegrity
	}
	raw, err := page.Encode(tree, keys, source)
	if err != nil {
		return result, err
	}
	hash := HashContent(raw)
	state.Directory, result.objects, err = state.Directory.Append(ctx, hash)
	if err != nil {
		return DocToolOrderStep{}, err
	}
	result.objects = append(result.objects, DocRangeObject{Hash: hash, Bytes: raw})
	state.Next += len(page.Entries)
	after := cloneDocToolOrderEntry(page.Entries[len(page.Entries)-1])
	state.After = &after
	if _, err := state.Encode(tree, keys, source); err != nil {
		return DocToolOrderStep{}, err
	}
	result.source, result.prior, result.state, result.entries = source, HashContent(encoded), state, page.Entries
	return result, nil
}

func FinishDocToolOrder(ctx context.Context, tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrderSource, state DocToolOrderState, load func(context.Context, ContentHash) ([]byte, error)) (plan DocToolOrderPlan, objects []DocRangeObject, failure error) {
	source.Parent = cloneDocToolTreeNode(source.Parent)
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			plan, objects = DocToolOrderPlan{}, nil
		}
	}()
	if err := validateDocToolOrderCheckpoint(ctx, tree, keys, source, state, load); err != nil {
		return plan, nil, err
	}
	if !state.Done(source) {
		return plan, nil, ErrIntegrity
	}
	root, produced, err := state.Directory.Finish(ctx)
	if err != nil {
		return plan, nil, err
	}
	plan = DocToolOrderPlan{Version: 1, Source: source, Pages: root}
	plan.Source.Parent = cloneDocToolTreeNode(plan.Source.Parent)
	if _, err := plan.Encode(tree, keys); err != nil {
		return DocToolOrderPlan{}, nil, err
	}
	return plan, produced, nil
}
