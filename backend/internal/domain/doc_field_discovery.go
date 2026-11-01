package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
)

const DocFieldDiscoveryStepBytes = 64 << 10
const MaxDocFieldDiscoveryStateBytes = 64 << 10
const docFieldDiscoveryDepth = 10000

type docFieldSchema byte

const (
	docFieldSchemaEvent docFieldSchema = iota
	docFieldSchemaBlock
	docFieldSchemaTool
)

const (
	docFieldObjectFirst byte = iota + 1
	docFieldObjectKey
	docFieldObjectColon
	docFieldObjectValue
	docFieldObjectAfter
	docFieldArrayFirst
	docFieldArrayValue
	docFieldArrayAfter
)

const (
	docFieldTokenNone byte = iota
	docFieldTokenKey
	docFieldTokenString
	docFieldTokenNumber
	docFieldTokenNull
	docFieldTokenTrue
	docFieldTokenFalse
)

const (
	docFieldUnknown byte = iota
	docFieldKind
	docFieldOutput
	docFieldSummary
	docFieldBlocks
	docFieldToolName
)

type DocFieldDiscoveryState struct {
	Version         int              `json:"version"`
	EventHash       ContentHash      `json:"event_hash"`
	EventBytes      int              `json:"event_bytes"`
	Next            int              `json:"next"`
	HashState       []byte           `json:"hash_state,omitempty"`
	Digest          ContentHash      `json:"digest,omitempty"`
	Stack           []byte           `json:"stack,omitempty"`
	Started         bool             `json:"started"`
	RootDone        bool             `json:"root_done"`
	Done            bool             `json:"done"`
	Token           byte             `json:"token"`
	Number          byte             `json:"number"`
	Literal         int              `json:"literal"`
	Escape          byte             `json:"escape"`
	HexDigits       int              `json:"hex_digits"`
	HexValue        uint32           `json:"hex_value"`
	UTF8Value       uint32           `json:"utf8_value"`
	UTF8Need        int              `json:"utf8_need"`
	UTF8Minimum     uint32           `json:"utf8_minimum"`
	StringPurpose   byte             `json:"string_purpose"`
	StringMask      byte             `json:"string_mask"`
	StringRunes     int              `json:"string_runes"`
	Field           byte             `json:"field"`
	ValueFrom       int              `json:"value_from"`
	ValueType       byte             `json:"value_type"`
	Kind            EventKind        `json:"kind"`
	Summary         *DocFindSpan     `json:"summary,omitempty"`
	Blocks          *DocFindSpan     `json:"blocks,omitempty"`
	InvalidSummary  bool             `json:"invalid_summary"`
	InvalidBlocks   bool             `json:"invalid_blocks"`
	Output          *DocFindSpan     `json:"output,omitempty"`
	OutputNonSpace  bool             `json:"output_non_space"`
	KindToken       *DocFindSpan     `json:"kind_token,omitempty"`
	ToolName        *DocFindSpan     `json:"tool_name,omitempty"`
	InvalidToolName bool             `json:"invalid_tool_name,omitempty"`
	ToolOutputType  DocToolValueType `json:"tool_output_type,omitempty"`
	ToolInputType   DocToolValueType `json:"tool_input_type,omitempty"`
	schema          docFieldSchema
}

func NewDocFieldDiscoveryState(event ContentHash, length int) (DocFieldDiscoveryState, error) {
	if ValidateContentHash(event) != nil || length <= 0 || length > MaxConversationDocumentBytes {
		return DocFieldDiscoveryState{}, ErrIntegrity
	}
	initial, err := sha256.New().(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return DocFieldDiscoveryState{}, err
	}
	if !validDocFieldHashState(initial, 0) {
		return DocFieldDiscoveryState{}, ErrIntegrity
	}
	return DocFieldDiscoveryState{Version: 1, EventHash: event, EventBytes: length, HashState: initial}, nil
}

func newDocBlockFieldParser(eventHash ContentHash, eventBytes, from int) DocFieldDiscoveryState {
	return DocFieldDiscoveryState{Version: 1, EventHash: eventHash, EventBytes: eventBytes, Next: from, schema: docFieldSchemaBlock}
}

func (state DocFieldDiscoveryState) Validate() error {
	return state.validateHashProgress(docFieldSchemaEvent)
}

func (state DocFieldDiscoveryState) validateHashProgress(schema docFieldSchema) error {
	if schema != docFieldSchemaEvent && schema != docFieldSchemaTool || state.schema != schema || ValidateContentHash(state.EventHash) != nil || state.Done != (state.Next == state.EventBytes) {
		return ErrIntegrity
	}
	if err := state.validateSyntax(schema); err != nil {
		return err
	}
	if state.Done {
		if !state.RootDone || state.Digest != state.EventHash || len(state.HashState) != 0 {
			return ErrIntegrity
		}
	} else {
		if state.Next%sha256.BlockSize != 0 || state.Digest != "" || !validDocFieldHashState(state.HashState, state.Next) {
			return ErrIntegrity
		}
		digest := sha256.New()
		if digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(state.HashState) != nil {
			return ErrIntegrity
		}
		raw, err := digest.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil || !bytes.Equal(raw, state.HashState) {
			return ErrIntegrity
		}
	}
	if state.Next == 0 {
		initial, err := NewDocFieldDiscoveryState(state.EventHash, state.EventBytes)
		if err != nil {
			return err
		}
		initial.schema = schema
		actual, _ := canonicalJSON(state)
		expected, _ := canonicalJSON(initial)
		if !bytes.Equal(actual, expected) {
			return ErrIntegrity
		}
	}
	return nil
}

func (state DocFieldDiscoveryState) validateSyntax(schema docFieldSchema) error {
	if schema != docFieldSchemaEvent && schema != docFieldSchemaBlock && schema != docFieldSchemaTool || state.schema != schema || schema != docFieldSchemaTool && (state.KindToken != nil || state.ToolName != nil || state.InvalidToolName || state.ToolOutputType != 0 || state.ToolInputType != 0) {
		return ErrIntegrity
	}
	keyMask := byte(1<<len(schema.keys())) - 1
	kindMask := byte(1<<len(schema.kinds())) - 1
	if state.Version != 1 || state.EventBytes <= 0 || state.EventBytes > MaxConversationDocumentBytes || state.Next < 0 || state.Next > state.EventBytes || len(state.Stack) > docFieldDiscoveryDepth || state.Token > docFieldTokenFalse || state.Number > 8 || state.Literal < 0 || state.Literal > 4 || state.Escape > 2 || state.HexDigits < 0 || state.HexDigits > 3 || state.HexValue > 0xffff || state.UTF8Need < 0 || state.UTF8Need > 3 || state.UTF8Value > 0x10ffff || state.StringPurpose > 2 || state.StringMask > keyMask || state.StringRunes < 0 || state.StringRunes > schema.maxStringRunes() || state.Field > byte(len(schema.keys())) || state.ValueFrom < 0 || state.ValueFrom > state.Next {
		return ErrIntegrity
	}
	for index, frame := range state.Stack {
		if frame < docFieldObjectFirst || frame > docFieldArrayAfter {
			return ErrIntegrity
		}
		if index < len(state.Stack)-1 && frame != docFieldObjectValue && frame != docFieldArrayFirst && frame != docFieldArrayValue {
			return ErrIntegrity
		}
	}
	if state.Started && (len(state.Stack) == 0) != state.RootDone || !state.Started && (len(state.Stack) != 0 || state.RootDone || state.Token != 0 || state.Field != 0 || state.Kind != "" || state.Summary != nil || state.Blocks != nil || state.Output != nil || state.OutputNonSpace || state.InvalidSummary || state.InvalidBlocks || state.KindToken != nil || state.ToolName != nil || state.InvalidToolName) || state.RootDone && (state.Token != 0 || state.Field != 0 || state.ValueFrom != 0 || state.ValueType != 0) {
		return ErrIntegrity
	}
	validKind := state.Kind == ""
	for _, kind := range schema.kinds() {
		validKind = validKind || state.Kind == EventKind(kind)
	}
	if len(state.Stack) > 0 && state.Stack[0] > docFieldObjectAfter || !validKind {
		return ErrIntegrity
	}
	stringToken := state.Token == docFieldTokenKey || state.Token == docFieldTokenString
	if !stringToken && (state.Escape != 0 || state.HexDigits != 0 || state.HexValue != 0 || state.UTF8Need != 0 || state.UTF8Value != 0 || state.UTF8Minimum != 0 || state.StringPurpose != 0 || state.StringMask != 0 || state.StringRunes != 0) || state.Token != docFieldTokenNumber && state.Number != 0 || state.Token < docFieldTokenNull && state.Literal != 0 {
		return ErrIntegrity
	}
	if state.Escape != 2 && (state.HexDigits != 0 || state.HexValue != 0) || state.Escape == 2 && state.HexValue >= uint32(1)<<(4*state.HexDigits) || state.UTF8Need > 0 && (state.Escape != 0 || state.UTF8Minimum != 0x80 && state.UTF8Minimum != 0x800 && state.UTF8Minimum != 0x10000) || state.UTF8Need == 0 && (state.UTF8Value != 0 || state.UTF8Minimum != 0) {
		return ErrIntegrity
	}
	if state.UTF8Need > 0 && !state.validUTF8Progress() {
		return ErrIntegrity
	}
	if state.Token != 0 && len(state.Stack) == 0 || state.Token == docFieldTokenNumber && (state.Number == 0 || state.Number > 8) {
		return ErrIntegrity
	}
	if state.Token != 0 {
		frame := state.Stack[len(state.Stack)-1]
		if state.Token == docFieldTokenKey {
			if frame != docFieldObjectFirst && frame != docFieldObjectKey {
				return ErrIntegrity
			}
		} else if frame != docFieldObjectValue && frame != docFieldArrayFirst && frame != docFieldArrayValue {
			return ErrIntegrity
		}
	}
	purpose := byte(0)
	if state.Token == docFieldTokenKey && len(state.Stack) == 1 {
		purpose = 1
	} else if state.Token == docFieldTokenString && len(state.Stack) == 1 && state.Field == docFieldKind {
		purpose = 2
	}
	if state.StringPurpose != purpose || purpose == 0 && (state.StringMask != 0 || state.StringRunes != 0) || purpose == 2 && state.StringMask > kindMask {
		return ErrIntegrity
	}
	if state.Token >= docFieldTokenNull && (state.Literal < 1 || state.Literal >= 4 && state.Token != docFieldTokenFalse) {
		return ErrIntegrity
	}
	for index, span := range []*DocFindSpan{state.Summary, state.Blocks, state.Output, state.KindToken, state.ToolName} {
		minimum := 2
		if schema == docFieldSchemaTool && index == 2 {
			minimum = 1
		}
		if span != nil && (span.From < 0 || span.From > state.Next || span.To < span.From || span.To > state.Next || span.To-span.From < minimum) {
			return ErrIntegrity
		}
	}
	if schema == docFieldSchemaTool {
		return state.validateToolFields()
	}
	return nil
}

func validDocFieldHashState(raw []byte, count int) bool {
	const size = 4 + sha256.Size + sha256.BlockSize + 8
	if len(raw) != size || string(raw[:4]) != "sha\x03" || count < 0 || count%sha256.BlockSize != 0 || binary.BigEndian.Uint64(raw[size-8:]) != uint64(count) {
		return false
	}
	for _, character := range raw[4+sha256.Size : size-8] {
		if character != 0 {
			return false
		}
	}
	return true
}

func (state DocFieldDiscoveryState) Encode() ([]byte, error) {
	if err := state.Validate(); err != nil {
		return nil, err
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocFieldDiscoveryStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocFieldDiscoveryState(expected ContentHash, raw []byte) (DocFieldDiscoveryState, error) {
	var state DocFieldDiscoveryState
	if len(raw) == 0 || len(raw) > MaxDocFieldDiscoveryStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocFieldDiscoveryState{}, ErrIntegrity
	}
	canonical, err := state.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocFieldDiscoveryState{}, ErrIntegrity
	}
	return state, nil
}

type DocFieldDiscoveryStep struct {
	previous ContentHash
	from     int
	raw      []byte
	hash     ContentHash
}

func (step DocFieldDiscoveryStep) Previous() ContentHash { return step.previous }
func (step DocFieldDiscoveryStep) From() int             { return step.from }

func (step DocFieldDiscoveryStep) Checkpoint() (ContentHash, []byte) {
	return step.hash, bytes.Clone(step.raw)
}

func (step DocFieldDiscoveryStep) State() (DocFieldDiscoveryState, error) {
	state, err := DecodeDocFieldDiscoveryState(step.hash, step.raw)
	if err != nil || state.Next <= step.from || state.Next-step.from > DocFieldDiscoveryStepBytes || step.from == 0 && step.previous != "" || step.from > 0 && ValidateContentHash(step.previous) != nil {
		return DocFieldDiscoveryState{}, ErrIntegrity
	}
	return state, nil
}

func AdvanceDocFieldDiscovery(ctx context.Context, prior DocFieldDiscoveryState, raw []byte) (DocFieldDiscoveryStep, error) {
	if err := ctx.Err(); err != nil {
		return DocFieldDiscoveryStep{}, err
	}
	if prior.Validate() != nil || prior.Done || len(raw) == 0 || len(raw) > DocFieldDiscoveryStepBytes || len(raw) > prior.EventBytes-prior.Next || prior.Next+len(raw) < prior.EventBytes && len(raw)%sha256.BlockSize != 0 {
		return DocFieldDiscoveryStep{}, ErrIntegrity
	}
	encoded, err := prior.Encode()
	if err != nil {
		return DocFieldDiscoveryStep{}, err
	}
	state, err := DecodeDocFieldDiscoveryState(HashContent(encoded), encoded)
	if err != nil {
		return DocFieldDiscoveryStep{}, err
	}
	if err := advanceDocFieldDiscoverySyntaxHash(ctx, &state, raw); err != nil {
		return DocFieldDiscoveryStep{}, err
	}
	checkpoint, err := state.Encode()
	if err != nil {
		return DocFieldDiscoveryStep{}, err
	}
	if err := ctx.Err(); err != nil {
		return DocFieldDiscoveryStep{}, err
	}
	previous := ContentHash("")
	if prior.Next > 0 {
		previous = HashContent(encoded)
	}
	return DocFieldDiscoveryStep{previous: previous, from: prior.Next, raw: checkpoint, hash: HashContent(checkpoint)}, nil
}

func advanceDocFieldDiscoverySyntaxHash(ctx context.Context, state *DocFieldDiscoveryState, raw []byte) error {
	digest := sha256.New()
	if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(state.HashState); err != nil {
		return ErrIntegrity
	}
	_, _ = digest.Write(raw)
	if err := state.consume(ctx, raw); err != nil {
		return err
	}
	state.Done = state.Next == state.EventBytes
	if state.Done {
		state.Digest = ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil)))
		state.HashState = nil
		return nil
	}
	var err error
	state.HashState, err = digest.(encoding.BinaryMarshaler).MarshalBinary()
	return err
}

func (state DocFieldDiscoveryState) DisplaySource() (DocDisplayStringSource, error) {
	if state.Validate() != nil || !state.Done {
		return DocDisplayStringSource{}, ErrIntegrity
	}
	if state.Kind != EventToolResult || state.Output == nil {
		return DocDisplayStringSource{}, ErrDocFindDisplayUnavailable
	}
	source := DocDisplayStringSource{Version: 1, Profile: DocDisplayStringProfile, EventHash: state.EventHash, EventBytes: state.EventBytes, Token: *state.Output, Empty: !state.OutputNonSpace}
	return source, source.Validate()
}

func (state DocFieldDiscoveryState) FindFields() (DocFindFields, error) {
	if state.Validate() != nil || !state.Done {
		return DocFindFields{}, ErrIntegrity
	}
	fields := DocFindFields{Version: 1, EventHash: state.EventHash, EventBytes: state.EventBytes}
	switch state.Kind {
	case EventMessage:
		if state.InvalidBlocks {
			return DocFindFields{}, ErrIntegrity
		}
		fields.Blocks = state.Blocks
	case EventReasoning:
		if state.InvalidSummary {
			return DocFindFields{}, ErrIntegrity
		}
		fields.Summary = state.Summary
	default:
		return DocFindFields{}, ErrIntegrity
	}
	return fields, nil
}
