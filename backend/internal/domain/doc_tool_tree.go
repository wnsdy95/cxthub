package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const DocToolTreeProfile = "public-tool-json-tree-v1"
const MaxDocToolTreeNodeBytes = 2048

type DocToolTreeSource struct {
	Version    int              `json:"version"`
	Profile    string           `json:"profile"`
	EventHash  ContentHash      `json:"event_hash"`
	EventBytes int              `json:"event_bytes"`
	Evidence   ContentHash      `json:"evidence"`
	Root       DocFindSpan      `json:"root"`
	Type       DocToolValueType `json:"type"`
}

func NewDocToolTreeSource(discoverySource DocToolFieldDiscoverySource, state DocToolFieldDiscoveryState, entry DocRangeEntry) (DocToolTreeSource, error) {
	fields, err := state.Fields(discoverySource)
	if err != nil || entry.Hash != discoverySource.EventHash || entry.Length != discoverySource.EventBytes || entry.Kind != fields.Kind {
		return DocToolTreeSource{}, ErrIntegrity
	}
	return docToolTreeSourceFromFields(discoverySource, fields)
}

func docToolTreeSourceFromFields(discoverySource DocToolFieldDiscoverySource, fields DocToolFields) (DocToolTreeSource, error) {
	raw, err := fields.Encode(discoverySource)
	if err != nil {
		return DocToolTreeSource{}, err
	}
	root, valueType := fields.Output, fields.OutputType
	if fields.Kind == EventToolCall {
		root, valueType = fields.Input, fields.InputType
	}
	if root == nil {
		return DocToolTreeSource{}, ErrDocFindDisplayUnavailable
	}
	source := DocToolTreeSource{Version: 1, Profile: DocToolTreeProfile, EventHash: fields.EventHash, EventBytes: fields.EventBytes, Evidence: HashContent(raw), Root: *root, Type: valueType}
	return source, source.Validate()
}

func (source DocToolTreeSource) Validate() error {
	if source.Version != 1 || source.Profile != DocToolTreeProfile || ValidateContentHash(source.EventHash) != nil || ValidateContentHash(source.Evidence) != nil || source.EventBytes <= 0 || source.EventBytes > MaxConversationDocumentBytes || source.Root.From < 0 || source.Root.To > source.EventBytes || source.Root.To <= source.Root.From || !docToolTreeValueLength(source.Type, source.Root.To-source.Root.From) {
		return ErrIntegrity
	}
	return nil
}

func (source DocToolTreeSource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	return json.Marshal(struct {
		EventBytes int              `json:"event_bytes"`
		EventHash  ContentHash      `json:"event_hash"`
		Evidence   ContentHash      `json:"evidence"`
		Profile    string           `json:"profile"`
		Root       DocFindSpan      `json:"root"`
		Type       DocToolValueType `json:"type"`
		Version    int              `json:"version"`
	}{EventBytes: source.EventBytes, EventHash: source.EventHash, Evidence: source.Evidence,
		Profile: source.Profile, Root: source.Root, Type: source.Type, Version: source.Version})
}

func (source DocToolTreeSource) Hash() (ContentHash, error) {
	raw, err := source.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func DecodeDocToolTreeSource(expected ContentHash, raw []byte) (DocToolTreeSource, error) {
	var source DocToolTreeSource
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &source) != nil {
		return DocToolTreeSource{}, ErrIntegrity
	}
	canonical, err := source.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolTreeSource{}, ErrIntegrity
	}
	return source, nil
}

func docToolTreeValueLength(valueType DocToolValueType, length int) bool {
	switch valueType {
	case DocToolValueObject, DocToolValueArray, DocToolValueString:
		return length >= 2
	case DocToolValueNumber:
		return length >= 1
	case DocToolValueNull, DocToolValueTrue:
		return length == 4
	case DocToolValueFalse:
		return length == 5
	default:
		return false
	}
}

type DocToolTreeNode struct {
	Value    DocFindSpan      `json:"value"`
	Type     DocToolValueType `json:"type"`
	Parent   int              `json:"parent"`
	Ordinal  int              `json:"ordinal"`
	Key      *DocFindSpan     `json:"key,omitempty"`
	Children int              `json:"children"`
	Depth    int              `json:"depth"`
}

func (node DocToolTreeNode) validatePosition(source DocToolTreeSource) error {
	if !node.Type.valid() || node.Value.From < source.Root.From || node.Value.From >= source.Root.To || node.Ordinal < 0 || node.Ordinal > source.Root.To-source.Root.From || node.Children < 0 || node.Children > source.Root.To-source.Root.From || node.Depth < 0 || node.Depth > docToolDisplayMaxDepth {
		return ErrIntegrity
	}
	if node.Type != DocToolValueObject && node.Type != DocToolValueArray && node.Children != 0 {
		return ErrIntegrity
	}
	if node.Depth == 0 {
		if node.Value.From != source.Root.From || node.Type != source.Type || node.Parent != -1 || node.Ordinal != 0 || node.Key != nil {
			return ErrIntegrity
		}
		return nil
	}
	if node.Parent < source.Root.From || node.Parent >= node.Value.From || node.Value.From == source.Root.From || node.Depth == 1 && node.Parent != source.Root.From || node.Depth > 1 && node.Parent == source.Root.From {
		return ErrIntegrity
	}
	anchor := node.Value.From
	if node.Key != nil {
		if node.Key.From <= node.Parent || node.Key.From >= node.Value.From || node.Key.To <= node.Key.From || node.Key.To >= node.Value.From || node.Key.To-node.Key.From < 2 {
			return ErrIntegrity
		}
		anchor = node.Key.From
	}
	if node.Ordinal > (anchor-node.Parent-1)/2 || node.Depth == 1 && (source.Type != DocToolValueObject && source.Type != DocToolValueArray || (node.Key != nil) != (source.Type == DocToolValueObject)) {
		return ErrIntegrity
	}
	return nil
}

func (node DocToolTreeNode) Validate(source DocToolTreeSource) error {
	if source.Validate() != nil || node.validatePosition(source) != nil || node.Value.To > source.Root.To || node.Value.To <= node.Value.From || !docToolTreeValueLength(node.Type, node.Value.To-node.Value.From) {
		return ErrIntegrity
	}
	if node.Depth == 0 && node.Value != source.Root || node.Depth > 0 && node.Value.To >= source.Root.To {
		return ErrIntegrity
	}
	width := 2
	if node.Type == DocToolValueObject {
		width = 5
	}
	if node.Children > (node.Value.To-node.Value.From-1)/width {
		return ErrIntegrity
	}
	return nil
}

func (node DocToolTreeNode) Encode(source DocToolTreeSource) ([]byte, error) {
	if node.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := json.Marshal(struct {
		Children int              `json:"children"`
		Depth    int              `json:"depth"`
		Key      *DocFindSpan     `json:"key,omitempty"`
		Ordinal  int              `json:"ordinal"`
		Parent   int              `json:"parent"`
		Type     DocToolValueType `json:"type"`
		Value    DocFindSpan      `json:"value"`
	}{Children: node.Children, Depth: node.Depth, Key: node.Key, Ordinal: node.Ordinal,
		Parent: node.Parent, Type: node.Type, Value: node.Value})
	if err != nil || len(raw) > MaxDocToolTreeNodeBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolTreeNode(source DocToolTreeSource, expected ContentHash, raw []byte) (DocToolTreeNode, error) {
	var node DocToolTreeNode
	if len(raw) == 0 || len(raw) > MaxDocToolTreeNodeBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &node) != nil {
		return DocToolTreeNode{}, ErrIntegrity
	}
	canonical, err := node.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolTreeNode{}, ErrIntegrity
	}
	return node, nil
}

func validateDocToolTreeEvidence(ctx context.Context, source DocToolTreeSource, load func(context.Context, ContentHash) ([]byte, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if source.Validate() != nil || load == nil {
		return ErrIntegrity
	}
	raw, err := load(ctx, source.Evidence)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	discoverySource, err := NewDocToolFieldDiscoverySource(source.EventHash, source.EventBytes)
	if err != nil {
		return err
	}
	fields, err := DecodeDocToolFields(discoverySource, source.Evidence, raw)
	if err != nil {
		return err
	}
	expected, err := docToolTreeSourceFromFields(discoverySource, fields)
	if err != nil || expected != source {
		return ErrIntegrity
	}
	return nil
}
