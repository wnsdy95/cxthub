package domain

import (
	"bytes"
	"encoding/json"
)

const DocToolValueStepActions = 64
const DocToolValueStepSourceBytes = 64 << 10
const DocToolValueStepOutputBytes = 64 << 10

type docToolValueFrame struct {
	Version  int              `json:"version"`
	Source   ContentHash      `json:"source"`
	Parent   ContentHash      `json:"parent,omitempty"`
	Plan     ContentHash      `json:"plan"`
	Node     DocToolTreeNode  `json:"node"`
	Next     int              `json:"next"`
	Previous int              `json:"previous"`
	Child    *DocToolTreeNode `json:"child,omitempty"`
}

func sameDocToolValueNode(tree DocToolTreeSource, left, right DocToolTreeNode) bool {
	actual, err := left.Encode(tree)
	expected, other := right.Encode(tree)
	return err == nil && other == nil && bytes.Equal(actual, expected)
}

func (frame docToolValueFrame) validate(source DocToolValueSource) error {
	hash, err := source.Hash()
	if err != nil || frame.Version != 1 || frame.Source != hash || frame.Node.Validate(source.Tree) != nil || frame.Node.Type != DocToolValueObject && frame.Node.Type != DocToolValueArray || ValidateContentHash(frame.Plan) != nil || frame.Next < 0 || frame.Next > frame.Node.Children || (frame.Next == 0) != (frame.Previous == 0) || frame.Next > 0 && (frame.Previous <= frame.Node.Value.From || frame.Previous >= frame.Node.Value.To) {
		return ErrIntegrity
	}
	if frame.Node.Depth == 0 {
		if frame.Parent != "" || !sameDocToolValueNode(source.Tree, frame.Node, source.Root) {
			return ErrIntegrity
		}
	} else if ValidateContentHash(frame.Parent) != nil {
		return ErrIntegrity
	}
	if frame.Child != nil && (frame.Next == frame.Node.Children || frame.Child.Validate(source.Tree) != nil || frame.Child.Parent != frame.Node.Value.From || frame.Child.Depth != frame.Node.Depth+1 || frame.Child.Value.To >= frame.Node.Value.To || frame.Child.Ordinal >= frame.Node.Children || (frame.Child.Key != nil) != (frame.Node.Type == DocToolValueObject)) {
		return ErrIntegrity
	}
	return nil
}

func (frame docToolValueFrame) encode(source DocToolValueSource) ([]byte, error) {
	if frame.validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(frame)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func decodeDocToolValueFrame(source DocToolValueSource, hash ContentHash, raw []byte) (docToolValueFrame, error) {
	var frame docToolValueFrame
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(hash) != nil || HashContent(raw) != hash || json.Unmarshal(raw, &frame) != nil {
		return docToolValueFrame{}, ErrIntegrity
	}
	encoded, err := frame.encode(source)
	if err != nil || !bytes.Equal(encoded, raw) {
		return docToolValueFrame{}, ErrIntegrity
	}
	return frame, nil
}

type DocToolValueState struct {
	Version   int                          `json:"version"`
	Source    ContentHash                  `json:"source"`
	Phase     string                       `json:"phase"`
	Value     *DocToolTreeNode             `json:"value,omitempty"`
	Frame     *docToolValueFrame           `json:"frame,omitempty"`
	Active    ContentHash                  `json:"active,omitempty"`
	Start     int                          `json:"start"`
	Offset    int                          `json:"offset"`
	TextBytes int                          `json:"text_bytes"`
	Directory DocRangeDirectoryAccumulator `json:"directory"`
	Pending   ContentHash                  `json:"pending,omitempty"`
	Receipt   ContentHash                  `json:"receipt,omitempty"`
}

func NewDocToolValueState(source DocToolValueSource) (DocToolValueState, error) {
	hash, err := source.Hash()
	if err != nil {
		return DocToolValueState{}, err
	}
	root := cloneDocToolTreeNode(source.Root)
	return DocToolValueState{Version: 1, Source: hash, Phase: "value", Value: &root}, nil
}

func (state DocToolValueState) Done() bool { return state.Phase == "complete" }

func (state DocToolValueState) Validate(source DocToolValueSource) error {
	hash, err := source.Hash()
	if err != nil || state.Version != 1 || state.Source != hash || state.TextBytes < 0 || state.TextBytes > MaxConversationDocumentBytes || state.Directory.Validate() != nil || int64(state.TextBytes) > int64(state.Directory.Count+1)*DocDisplayPageTextBytes || state.Directory.Count > 0 && (state.TextBytes == 0 || state.Directory.Count-1 > (state.TextBytes-1)/DocToolValuePageMinRunes) || state.Pending != "" && ValidateContentHash(state.Pending) != nil || (state.Directory.Count == 0 && state.Pending == "") != (state.TextBytes == 0) || state.Start < 0 || state.Start > state.TextBytes || state.Active == "" && state.Start != 0 || state.Active != "" && ValidateContentHash(state.Active) != nil {
		return ErrIntegrity
	}
	if state.Receipt == "" {
		initial, err := NewDocToolValueState(source)
		actual, actualErr := canonicalJSON(state)
		expected, expectedErr := canonicalJSON(initial)
		if err != nil || actualErr != nil || expectedErr != nil || !bytes.Equal(actual, expected) {
			return ErrIntegrity
		}
	} else if ValidateContentHash(state.Receipt) != nil {
		return ErrIntegrity
	}
	if state.Frame != nil && state.Frame.validate(source) != nil {
		return ErrIntegrity
	}
	if state.Value != nil {
		if state.Value.Validate(source.Tree) != nil || state.Frame == nil && !sameDocToolValueNode(source.Tree, *state.Value, source.Root) || state.Frame != nil && (state.Frame.Child == nil || !sameDocToolValueNode(source.Tree, *state.Value, *state.Frame.Child)) {
			return ErrIntegrity
		}
	}
	if state.Phase != "colon" && state.Offset != 0 || state.Phase != "key" && state.Phase != "string" && state.Phase != "number" && state.Active != "" {
		return ErrIntegrity
	}
	switch state.Phase {
	case "value", "string", "number":
		if state.Value == nil || state.Phase == "string" && (state.Value.Type != DocToolValueString || state.Active == "") || state.Phase == "number" && (state.Value.Type != DocToolValueNumber || state.Active == "") {
			return ErrIntegrity
		}
	case "next":
		if state.Value != nil || state.Frame == nil || state.Frame.Child != nil {
			return ErrIntegrity
		}
	case "key", "colon":
		if state.Value != nil || state.Frame == nil || state.Frame.Child == nil || state.Frame.Node.Type != DocToolValueObject || state.Frame.Child.Key == nil || state.Phase == "colon" && (state.Offset < state.Frame.Child.Key.To || state.Offset >= state.Frame.Child.Value.From) {
			return ErrIntegrity
		}
	case "complete":
		if state.Value != nil || state.Frame != nil || state.Pending != "" || state.Directory.Count == 0 {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	return nil
}

func (state DocToolValueState) Encode(source DocToolValueSource) ([]byte, error) {
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolValueState(source DocToolValueSource, hash ContentHash, raw []byte) (DocToolValueState, error) {
	var state DocToolValueState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(hash) != nil || HashContent(raw) != hash || json.Unmarshal(raw, &state) != nil {
		return DocToolValueState{}, ErrIntegrity
	}
	encoded, err := state.Encode(source)
	if err != nil || !bytes.Equal(encoded, raw) {
		return DocToolValueState{}, ErrIntegrity
	}
	return state, nil
}

func cloneDocToolValueState(state DocToolValueState) DocToolValueState {
	if levels := state.Directory.Levels; levels != nil {
		state.Directory.Levels = make([][]ContentHash, len(levels))
		for index, hashes := range levels {
			if hashes != nil {
				state.Directory.Levels[index] = make([]ContentHash, len(hashes))
				copy(state.Directory.Levels[index], hashes)
			}
		}
	}
	if state.Value != nil {
		value := cloneDocToolTreeNode(*state.Value)
		state.Value = &value
	}
	if state.Frame != nil {
		frame := *state.Frame
		frame.Node = cloneDocToolTreeNode(frame.Node)
		if frame.Child != nil {
			child := cloneDocToolTreeNode(*frame.Child)
			frame.Child = &child
		}
		state.Frame = &frame
	}
	return state
}

func (state DocToolValueState) controlDigest() (ContentHash, error) {
	state.Receipt = ""
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return "", ErrIntegrity
	}
	return HashContent(raw), nil
}

type docToolValueReceipt struct {
	Version int         `json:"version"`
	Profile string      `json:"profile"`
	Source  ContentHash `json:"source"`
	Digest  ContentHash `json:"digest"`
}

func (builder *docToolValueBuilder) recordReceipt() error {
	digest, err := builder.state.controlDigest()
	if err != nil {
		return err
	}
	receipt := docToolValueReceipt{Version: 1, Profile: "public-tool-value-receipt-v1", Source: builder.state.Source, Digest: digest}
	raw, err := canonicalJSON(receipt)
	if err != nil {
		return err
	}
	builder.state.Receipt, err = builder.buffer.put(raw, nil)
	return err
}
