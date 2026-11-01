package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const DocToolOrdersProfile = "public-tool-orders-v1"

type DocToolOrdersSource struct {
	Version int         `json:"version"`
	Profile string      `json:"profile"`
	Tree    ContentHash `json:"tree"`
	Keys    ContentHash `json:"keys"`
	Nodes   int         `json:"nodes"`
}

func NewDocToolOrdersSource(tree DocToolTreeSource, keys DocToolKeyMapSource) (DocToolOrdersSource, error) {
	treeHash, err := tree.Hash()
	keyHash, keyErr := keys.Hash(tree)
	if err != nil || keyErr != nil {
		return DocToolOrdersSource{}, ErrIntegrity
	}
	return DocToolOrdersSource{Version: 1, Profile: DocToolOrdersProfile, Tree: treeHash, Keys: keyHash, Nodes: keys.Nodes}, nil
}

func (source DocToolOrdersSource) Validate(tree DocToolTreeSource, keys DocToolKeyMapSource) error {
	expected, err := NewDocToolOrdersSource(tree, keys)
	if err != nil || source != expected {
		return ErrIntegrity
	}
	return nil
}

func (source DocToolOrdersSource) Encode(tree DocToolTreeSource, keys DocToolKeyMapSource) ([]byte, error) {
	if source.Validate(tree, keys) != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(source)
}

func (source DocToolOrdersSource) Hash(tree DocToolTreeSource, keys DocToolKeyMapSource) (ContentHash, error) {
	raw, err := source.Encode(tree, keys)
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

type DocToolOrdersState struct {
	Version    int                 `json:"version"`
	Source     ContentHash         `json:"source"`
	After      int                 `json:"after"`
	Seen       int                 `json:"seen"`
	Containers int                 `json:"containers"`
	Current    *DocToolOrderSource `json:"current,omitempty"`
	Active     ContentHash         `json:"active,omitempty"`
}

func NewDocToolOrdersState(tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrdersSource) (DocToolOrdersState, error) {
	hash, err := source.Hash(tree, keys)
	if err != nil {
		return DocToolOrdersState{}, err
	}
	return DocToolOrdersState{Version: 1, Source: hash, After: tree.Root.From - 1}, nil
}

func (state DocToolOrdersState) Done(source DocToolOrdersSource) bool {
	return state.Seen == source.Nodes
}

func (state DocToolOrdersState) Validate(tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrdersSource) error {
	hash, err := source.Hash(tree, keys)
	if err != nil || state.Version != 1 || state.Source != hash || state.After < tree.Root.From-1 || state.After >= tree.Root.To || state.Seen < 0 || state.Seen > source.Nodes || state.Containers < 0 || state.Containers > state.Seen || (state.Seen == 0) != (state.After == tree.Root.From-1) || (state.Current == nil) != (state.Active == "") {
		return ErrIntegrity
	}
	if state.Seen > 0 && ((state.After == tree.Root.From) != (state.Seen == 1) || (tree.Type == DocToolValueObject || tree.Type == DocToolValueArray) && state.Containers == 0) {
		return ErrIntegrity
	}
	if state.Current != nil && (state.Done(source) || state.Current.Validate(tree, keys) != nil || state.Current.Parent.Value.From <= state.After || ValidateContentHash(state.Active) != nil) {
		return ErrIntegrity
	}
	return nil
}

func (state DocToolOrdersState) Encode(tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrdersSource) ([]byte, error) {
	if state.Validate(tree, keys, source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolOrdersState(tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrdersSource, expected ContentHash, raw []byte) (DocToolOrdersState, error) {
	var state DocToolOrdersState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocToolOrdersState{}, ErrIntegrity
	}
	canonical, err := state.Encode(tree, keys, source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolOrdersState{}, ErrIntegrity
	}
	return state, nil
}

type DocToolPreparationReader struct {
	Nodes        DocToolTreeNodeLoader
	OrderSource  func(context.Context, int) (DocToolOrderSource, error)
	OrderEntries func(context.Context, DocToolOrderSource, *DocToolOrderEntry, int) ([]DocToolOrderEntry, error)
	OrderPlan    func(context.Context, int) (DocToolOrderPlan, error)
}

type DocToolOrdersStep struct {
	source  DocToolOrdersSource
	prior   ContentHash
	state   DocToolOrdersState
	inputs  []DocToolTreeNode
	order   *DocToolOrderStep
	plan    *DocToolOrderPlan
	objects []DocRangeObject
}

func (step DocToolOrdersStep) Source() DocToolOrdersSource { return step.source }
func (step DocToolOrdersStep) Prior() ContentHash          { return step.prior }

func (step DocToolOrdersStep) State() DocToolOrdersState {
	state := step.state
	if state.Current != nil {
		current := *state.Current
		current.Parent = cloneDocToolTreeNode(current.Parent)
		state.Current = &current
	}
	return state
}

func (step DocToolOrdersStep) Inputs() []DocToolTreeNode {
	result := make([]DocToolTreeNode, len(step.inputs))
	for index, node := range step.inputs {
		result[index] = cloneDocToolTreeNode(node)
	}
	return result
}

func (step DocToolOrdersStep) Order() (DocToolOrderStep, bool) {
	if step.order == nil {
		return DocToolOrderStep{}, false
	}
	return *step.order, true
}

func (step DocToolOrdersStep) Plan() (DocToolOrderPlan, bool) {
	if step.plan == nil {
		return DocToolOrderPlan{}, false
	}
	plan := *step.plan
	plan.Source.Parent = cloneDocToolTreeNode(plan.Source.Parent)
	return plan, true
}

func (step DocToolOrdersStep) Objects() []DocRangeObject {
	result := make([]DocRangeObject, len(step.objects))
	for index, object := range step.objects {
		result[index] = DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)}
	}
	return result
}
