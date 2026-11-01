package domain

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
)

const DocToolKeyMapProfile = "public-tool-key-map-v1"
const DocToolKeyMapStepNodes = 16

type DocToolKeyMapSource struct {
	Version int         `json:"version"`
	Profile string      `json:"profile"`
	Tree    ContentHash `json:"tree"`
	Nodes   int         `json:"nodes"`
}

func NewDocToolKeyMapSource(tree DocToolTreeSource, complete DocToolTreeScanState) (DocToolKeyMapSource, error) {
	hash, err := tree.Hash()
	if err != nil || complete.Validate(tree) != nil || !complete.Done() {
		return DocToolKeyMapSource{}, ErrIntegrity
	}
	source := DocToolKeyMapSource{Version: 1, Profile: DocToolKeyMapProfile, Tree: hash, Nodes: complete.NodeCount}
	return source, source.Validate(tree)
}

func (source DocToolKeyMapSource) Validate(tree DocToolTreeSource) error {
	hash, err := tree.Hash()
	if err != nil || source.Version != 1 || source.Profile != DocToolKeyMapProfile || source.Tree != hash || source.Nodes <= 0 || source.Nodes > tree.Root.To-tree.Root.From {
		return ErrIntegrity
	}
	return nil
}

func (source DocToolKeyMapSource) Encode(tree DocToolTreeSource) ([]byte, error) {
	if source.Validate(tree) != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(source)
}

func (source DocToolKeyMapSource) Hash(tree DocToolTreeSource) (ContentHash, error) {
	raw, err := source.Encode(tree)
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

type DocToolKeyItem struct {
	Node         DocToolTreeNode `json:"node"`
	Digest       ContentHash     `json:"digest"`
	IsArrayIndex bool            `json:"is_array_index"`
	ArrayIndex   uint32          `json:"array_index"`
}

func (item DocToolKeyItem) Validate(tree DocToolTreeSource) error {
	if item.Node.Validate(tree) != nil || item.Node.Key == nil || ValidateContentHash(item.Digest) != nil || !item.IsArrayIndex && item.ArrayIndex != 0 || item.IsArrayIndex && item.ArrayIndex > docToolKeyDigestMaxArrayIndex {
		return ErrIntegrity
	}
	return nil
}

type DocToolKeyMember struct {
	First int            `json:"first"`
	Last  DocToolKeyItem `json:"last"`
}

func (member DocToolKeyMember) Validate(tree DocToolTreeSource) error {
	if member.Last.Validate(tree) != nil || member.First <= member.Last.Node.Parent || member.First > member.Last.Node.Value.From {
		return ErrIntegrity
	}
	return nil
}

func MergeDocToolKeyMember(tree DocToolTreeSource, prior *DocToolKeyMember, item DocToolKeyItem) (DocToolKeyMember, error) {
	if item.Validate(tree) != nil {
		return DocToolKeyMember{}, ErrIntegrity
	}
	item.Node = cloneDocToolTreeNode(item.Node)
	if prior == nil {
		return DocToolKeyMember{First: item.Node.Value.From, Last: item}, nil
	}
	if prior.Validate(tree) != nil || prior.Last.Node.Parent != item.Node.Parent || prior.Last.Digest != item.Digest || prior.Last.IsArrayIndex != item.IsArrayIndex || prior.Last.ArrayIndex != item.ArrayIndex {
		return DocToolKeyMember{}, ErrIntegrity
	}
	result := *prior
	result.Last.Node = cloneDocToolTreeNode(result.Last.Node)
	result.First = min(result.First, item.Node.Value.From)
	if item.Node.Value.From == result.Last.Node.Value.From {
		left, _ := result.Last.Node.Encode(tree)
		right, _ := item.Node.Encode(tree)
		if !bytes.Equal(left, right) {
			return DocToolKeyMember{}, ErrIntegrity
		}
	} else if item.Node.Value.From > result.Last.Node.Value.From {
		result.Last = item
	}
	return result, nil
}

func CompareDocToolKeyMembers(left, right DocToolKeyMember) int {
	if order := cmp.Compare(left.Last.Node.Parent, right.Last.Node.Parent); order != 0 {
		return order
	}
	if left.Last.IsArrayIndex != right.Last.IsArrayIndex {
		if left.Last.IsArrayIndex {
			return -1
		}
		return 1
	}
	if left.Last.IsArrayIndex {
		if order := cmp.Compare(left.Last.ArrayIndex, right.Last.ArrayIndex); order != 0 {
			return order
		}
	}
	if order := cmp.Compare(left.First, right.First); order != 0 {
		return order
	}
	return cmp.Compare(left.Last.Digest, right.Last.Digest)
}

type DocToolKeyMapState struct {
	Version int              `json:"version"`
	Source  ContentHash      `json:"source"`
	Phase   string           `json:"phase"`
	After   int              `json:"after"`
	Seen    int              `json:"seen"`
	Keys    int              `json:"keys"`
	Current *DocToolTreeNode `json:"current,omitempty"`
	Active  ContentHash      `json:"active,omitempty"`
	Plan    ContentHash      `json:"plan,omitempty"`
}

func NewDocToolKeyMapState(tree DocToolTreeSource, source DocToolKeyMapSource) (DocToolKeyMapState, error) {
	hash, err := source.Hash(tree)
	if err != nil {
		return DocToolKeyMapState{}, err
	}
	return DocToolKeyMapState{Version: 1, Source: hash, Phase: "scan", After: tree.Root.From - 1}, nil
}

func (state DocToolKeyMapState) Done() bool { return state.Phase == "done" }

func (state DocToolKeyMapState) Validate(tree DocToolTreeSource, source DocToolKeyMapSource) error {
	hash, err := source.Hash(tree)
	if err != nil || state.Version != 1 || state.Source != hash || state.Seen < 0 || state.Seen > source.Nodes || state.Keys < 0 || state.Keys > max(0, state.Seen-1) || state.After < tree.Root.From-1 || state.After >= tree.Root.To || (state.Seen == 0) != (state.After == tree.Root.From-1) || state.Done() != (state.Seen == source.Nodes) {
		return ErrIntegrity
	}
	for _, hash := range []ContentHash{state.Active, state.Plan} {
		if hash != "" && ValidateContentHash(hash) != nil {
			return ErrIntegrity
		}
	}
	switch state.Phase {
	case "scan", "done":
		if state.Current != nil || state.Active != "" || state.Plan != "" {
			return ErrIntegrity
		}
	case "string", "digest":
		if state.Current == nil || state.Current.Validate(tree) != nil || state.Current.Key == nil || state.Current.Value.From <= state.After || state.Seen == 0 || state.Active == "" || (state.Plan != "") != (state.Phase == "digest") {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	return nil
}

func (state DocToolKeyMapState) Encode(tree DocToolTreeSource, source DocToolKeyMapSource) ([]byte, error) {
	if state.Validate(tree, source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolKeyMapState(tree DocToolTreeSource, source DocToolKeyMapSource, expected ContentHash, raw []byte) (DocToolKeyMapState, error) {
	var state DocToolKeyMapState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocToolKeyMapState{}, ErrIntegrity
	}
	canonical, err := state.Encode(tree, source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolKeyMapState{}, ErrIntegrity
	}
	return state, nil
}

type DocToolTreeNodeLoader func(context.Context, int, int) ([]DocToolTreeNode, error)

type DocToolKeyMapStep struct {
	source    DocToolKeyMapSource
	prior     ContentHash
	state     DocToolKeyMapState
	inputs    []DocToolTreeNode
	items     []DocToolKeyItem
	objects   []DocRangeObject
	readBytes int
}

func (step DocToolKeyMapStep) Source() DocToolKeyMapSource { return step.source }
func (step DocToolKeyMapStep) Prior() ContentHash          { return step.prior }
func (step DocToolKeyMapStep) ReadBytes() int              { return step.readBytes }

func cloneDocToolTreeNode(node DocToolTreeNode) DocToolTreeNode {
	node.Key = cloneDocToolFieldSpan(node.Key)
	return node
}

func (step DocToolKeyMapStep) State() DocToolKeyMapState {
	state := step.state
	if state.Current != nil {
		node := cloneDocToolTreeNode(*state.Current)
		state.Current = &node
	}
	return state
}

func (step DocToolKeyMapStep) Inputs() []DocToolTreeNode {
	result := make([]DocToolTreeNode, len(step.inputs))
	for index, node := range step.inputs {
		result[index] = cloneDocToolTreeNode(node)
	}
	return result
}

func (step DocToolKeyMapStep) Items() []DocToolKeyItem {
	result := make([]DocToolKeyItem, len(step.items))
	for index, item := range step.items {
		item.Node = cloneDocToolTreeNode(item.Node)
		result[index] = item
	}
	return result
}

func (step DocToolKeyMapStep) Objects() []DocRangeObject {
	result := make([]DocRangeObject, len(step.objects))
	for index, object := range step.objects {
		result[index] = DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)}
	}
	return result
}
