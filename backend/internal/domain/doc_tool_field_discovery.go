package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
)

const DocToolFieldDiscoveryProfile = "public-tool-field-discovery-v1"

var docToolFieldKeys = [...]string{"kind", "output", "input", "provider_tool_name", "tool_name"}
var docToolFieldKinds = [...]string{string(EventToolCall), string(EventToolResult)}

type DocToolValueType byte

const (
	DocToolValueObject DocToolValueType = '{'
	DocToolValueArray  DocToolValueType = '['
	DocToolValueString DocToolValueType = '"'
	DocToolValueNumber DocToolValueType = '0'
	DocToolValueNull   DocToolValueType = 'n'
	DocToolValueTrue   DocToolValueType = 't'
	DocToolValueFalse  DocToolValueType = 'f'
)

func (value DocToolValueType) valid() bool {
	switch value {
	case DocToolValueObject, DocToolValueArray, DocToolValueString, DocToolValueNumber, DocToolValueNull, DocToolValueTrue, DocToolValueFalse:
		return true
	default:
		return false
	}
}

func (state *DocFieldDiscoveryState) startToolFieldValue(character byte) {
	switch state.Field {
	case docFieldKind:
		state.Kind, state.KindToken = "", nil
	case docFieldOutput:
		state.Output, state.ToolOutputType = nil, 0
	case docFieldSummary:
		state.Summary, state.ToolInputType = nil, 0
		state.InvalidSummary = character != '{' && character != 'n'
	case docFieldBlocks:
		state.Blocks, state.InvalidBlocks = nil, character != '"' && character != 'n'
	case docFieldToolName:
		state.ToolName, state.InvalidToolName = nil, character != '"' && character != 'n'
	}
}

func (state *DocFieldDiscoveryState) finishToolFieldValue(span *DocFindSpan) {
	switch state.Field {
	case docFieldKind:
		if state.ValueType == '"' {
			state.KindToken = span
		}
	case docFieldOutput:
		state.Output, state.ToolOutputType = span, DocToolValueType(state.ValueType)
		if state.ValueType == '-' || state.ValueType >= '0' && state.ValueType <= '9' {
			state.ToolOutputType = DocToolValueNumber
		}
	case docFieldSummary:
		if state.ValueType == '{' || state.ValueType == 'n' {
			state.Summary, state.ToolInputType = span, DocToolValueType(state.ValueType)
		}
	case docFieldBlocks:
		if state.ValueType == '"' {
			state.Blocks = span
		}
	case docFieldToolName:
		if state.ValueType == '"' {
			state.ToolName = span
		}
	}
}

func (state DocFieldDiscoveryState) validateToolFields() error {
	if state.OutputNonSpace || state.Kind != "" && state.KindToken == nil || state.InvalidSummary && state.Summary != nil || state.InvalidBlocks && state.Blocks != nil || state.InvalidToolName && state.ToolName != nil || (state.Output == nil) != (state.ToolOutputType == 0) || state.Output != nil && !state.ToolOutputType.valid() || (state.Summary == nil) != (state.ToolInputType == 0) || state.Summary != nil && state.ToolInputType != DocToolValueObject && state.ToolInputType != DocToolValueNull {
		return ErrIntegrity
	}
	return nil
}

type DocToolFieldDiscoverySource struct {
	Version    int         `json:"version"`
	Profile    string      `json:"profile"`
	EventHash  ContentHash `json:"event_hash"`
	EventBytes int         `json:"event_bytes"`
}

func NewDocToolFieldDiscoverySource(event ContentHash, length int) (DocToolFieldDiscoverySource, error) {
	source := DocToolFieldDiscoverySource{Version: 1, Profile: DocToolFieldDiscoveryProfile, EventHash: event, EventBytes: length}
	if source.Validate() != nil {
		return DocToolFieldDiscoverySource{}, ErrIntegrity
	}
	return source, nil
}

func (source DocToolFieldDiscoverySource) Validate() error {
	if source.Version != 1 || source.Profile != DocToolFieldDiscoveryProfile || ValidateContentHash(source.EventHash) != nil || source.EventBytes <= 0 || source.EventBytes > MaxConversationDocumentBytes {
		return ErrIntegrity
	}
	return nil
}

func (source DocToolFieldDiscoverySource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(source)
}

func (source DocToolFieldDiscoverySource) Hash() (ContentHash, error) {
	raw, err := source.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func DecodeDocToolFieldDiscoverySource(expected ContentHash, raw []byte) (DocToolFieldDiscoverySource, error) {
	var source DocToolFieldDiscoverySource
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &source) != nil {
		return DocToolFieldDiscoverySource{}, ErrIntegrity
	}
	canonical, err := source.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolFieldDiscoverySource{}, ErrIntegrity
	}
	return source, nil
}

type DocToolFieldDiscoveryState struct {
	Version int                    `json:"version"`
	Source  ContentHash            `json:"source"`
	Syntax  DocFieldDiscoveryState `json:"syntax"`
}

func NewDocToolFieldDiscoveryState(source DocToolFieldDiscoverySource) (DocToolFieldDiscoveryState, error) {
	hash, err := source.Hash()
	if err != nil {
		return DocToolFieldDiscoveryState{}, err
	}
	syntax, err := NewDocFieldDiscoveryState(source.EventHash, source.EventBytes)
	if err != nil {
		return DocToolFieldDiscoveryState{}, err
	}
	syntax.schema = docFieldSchemaTool
	return DocToolFieldDiscoveryState{Version: 1, Source: hash, Syntax: syntax}, nil
}

func (state DocToolFieldDiscoveryState) Validate(source DocToolFieldDiscoverySource) error {
	hash, err := source.Hash()
	if err != nil || state.Version != 1 || state.Source != hash || state.Syntax.EventHash != source.EventHash || state.Syntax.EventBytes != source.EventBytes || state.Syntax.validateHashProgress(docFieldSchemaTool) != nil {
		return ErrIntegrity
	}
	return nil
}

func (state DocToolFieldDiscoveryState) Encode(source DocToolFieldDiscoverySource) ([]byte, error) {
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocFieldDiscoveryStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolFieldDiscoveryState(source DocToolFieldDiscoverySource, expected ContentHash, raw []byte) (DocToolFieldDiscoveryState, error) {
	var state DocToolFieldDiscoveryState
	if len(raw) == 0 || len(raw) > MaxDocFieldDiscoveryStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocToolFieldDiscoveryState{}, ErrIntegrity
	}
	state.Syntax.schema = docFieldSchemaTool
	canonical, err := state.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolFieldDiscoveryState{}, ErrIntegrity
	}
	return state, nil
}

type DocToolFieldDiscoveryStep struct {
	source   ContentHash
	previous ContentHash
	from     int
	raw      []byte
	hash     ContentHash
}

func (step DocToolFieldDiscoveryStep) Previous() ContentHash { return step.previous }
func (step DocToolFieldDiscoveryStep) From() int             { return step.from }

func (step DocToolFieldDiscoveryStep) Checkpoint() (ContentHash, []byte) {
	return step.hash, bytes.Clone(step.raw)
}

func (step DocToolFieldDiscoveryStep) State(source DocToolFieldDiscoverySource) (DocToolFieldDiscoveryState, error) {
	state, err := DecodeDocToolFieldDiscoveryState(source, step.hash, step.raw)
	if err != nil || state.Source != step.source || step.from < 0 || state.Syntax.Next <= step.from || state.Syntax.Next-step.from > DocFieldDiscoveryStepBytes || step.from == 0 && step.previous != "" || step.from > 0 && ValidateContentHash(step.previous) != nil {
		return DocToolFieldDiscoveryState{}, ErrIntegrity
	}
	return state, nil
}

func AdvanceDocToolFieldDiscovery(ctx context.Context, source DocToolFieldDiscoverySource, prior DocToolFieldDiscoveryState, raw []byte) (result DocToolFieldDiscoveryStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolFieldDiscoveryStep{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if prior.Validate(source) != nil || prior.Syntax.Done || len(raw) == 0 || len(raw) > DocFieldDiscoveryStepBytes || len(raw) > source.EventBytes-prior.Syntax.Next || prior.Syntax.Next+len(raw) < source.EventBytes && len(raw)%sha256.BlockSize != 0 {
		return result, ErrIntegrity
	}
	encoded, err := prior.Encode(source)
	if err != nil {
		return result, err
	}
	state, err := DecodeDocToolFieldDiscoveryState(source, HashContent(encoded), encoded)
	if err != nil {
		return result, err
	}
	if err := advanceDocFieldDiscoverySyntaxHash(ctx, &state.Syntax, raw); err != nil {
		return result, err
	}
	checkpoint, err := state.Encode(source)
	if err != nil {
		return result, err
	}
	previous := ContentHash("")
	if prior.Syntax.Next > 0 {
		previous = HashContent(encoded)
	}
	return DocToolFieldDiscoveryStep{source: prior.Source, previous: previous, from: prior.Syntax.Next, raw: checkpoint, hash: HashContent(checkpoint)}, nil
}

type DocToolFields struct {
	Version          int              `json:"version"`
	Source           ContentHash      `json:"source"`
	EventHash        ContentHash      `json:"event_hash"`
	EventBytes       int              `json:"event_bytes"`
	Kind             EventKind        `json:"kind"`
	KindToken        DocFindSpan      `json:"kind_token"`
	Output           *DocFindSpan     `json:"output,omitempty"`
	OutputType       DocToolValueType `json:"output_type,omitempty"`
	Input            *DocFindSpan     `json:"input,omitempty"`
	InputType        DocToolValueType `json:"input_type,omitempty"`
	ProviderToolName *DocFindSpan     `json:"provider_tool_name,omitempty"`
	ToolName         *DocFindSpan     `json:"tool_name,omitempty"`
}

func (state DocToolFieldDiscoveryState) Fields(source DocToolFieldDiscoverySource) (DocToolFields, error) {
	if state.Validate(source) != nil || !state.Syntax.Done || state.Syntax.KindToken == nil {
		return DocToolFields{}, ErrIntegrity
	}
	syntax := state.Syntax
	fields := DocToolFields{Version: 1, Source: state.Source, EventHash: source.EventHash, EventBytes: source.EventBytes, Kind: syntax.Kind, KindToken: *syntax.KindToken, Output: cloneDocToolFieldSpan(syntax.Output), OutputType: syntax.ToolOutputType}
	if syntax.Kind == EventToolCall {
		if syntax.InvalidSummary || syntax.InvalidBlocks || syntax.InvalidToolName {
			return DocToolFields{}, ErrIntegrity
		}
		fields.Input, fields.InputType = cloneDocToolFieldSpan(syntax.Summary), syntax.ToolInputType
		fields.ProviderToolName, fields.ToolName = cloneDocToolFieldSpan(syntax.Blocks), cloneDocToolFieldSpan(syntax.ToolName)
	}
	if fields.Validate(source) != nil {
		return DocToolFields{}, ErrIntegrity
	}
	return fields, nil
}

func (fields DocToolFields) Validate(source DocToolFieldDiscoverySource) error {
	hash, err := source.Hash()
	if err != nil || fields.Version != 1 || fields.Source != hash || fields.EventHash != source.EventHash || fields.EventBytes != source.EventBytes || fields.Kind != EventToolCall && fields.Kind != EventToolResult || (fields.Output == nil) != (fields.OutputType == 0) || fields.Output != nil && !fields.OutputType.valid() || (fields.Input == nil) != (fields.InputType == 0) || fields.Input != nil && fields.InputType != DocToolValueObject && fields.InputType != DocToolValueNull || fields.Kind == EventToolResult && (fields.Input != nil || fields.InputType != 0 || fields.ProviderToolName != nil || fields.ToolName != nil) {
		return ErrIntegrity
	}
	spans := [...]*DocFindSpan{&fields.KindToken, fields.Output, fields.Input, fields.ProviderToolName, fields.ToolName}
	for index, span := range spans {
		minimum := 2
		if index == 1 {
			minimum = 1
		}
		if span == nil {
			continue
		}
		if span.From < 0 || span.From > source.EventBytes-minimum || span.To <= span.From || span.To > source.EventBytes || span.To-span.From < minimum {
			return ErrIntegrity
		}
		for _, previous := range spans[:index] {
			if previous != nil && span.From < previous.To && previous.From < span.To {
				return ErrIntegrity
			}
		}
	}
	return nil
}

func (fields DocToolFields) Encode(source DocToolFieldDiscoverySource) ([]byte, error) {
	if fields.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(fields)
	if err != nil || len(raw) > MaxDocFindFieldsBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolFields(source DocToolFieldDiscoverySource, expected ContentHash, raw []byte) (DocToolFields, error) {
	var fields DocToolFields
	if len(raw) == 0 || len(raw) > MaxDocFindFieldsBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &fields) != nil {
		return DocToolFields{}, ErrIntegrity
	}
	canonical, err := fields.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolFields{}, ErrIntegrity
	}
	return fields, nil
}

func cloneDocToolFieldSpan(span *DocFindSpan) *DocFindSpan {
	if span == nil {
		return nil
	}
	cloned := *span
	return &cloned
}
