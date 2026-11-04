package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const DocToolTreeStepNodes = 16
const DocToolTreeStepFrames = 16
const DocToolTreeStepObjects = 32
const DocToolTreeStepSourceBytes = 64 << 10
const MaxDocToolTreeScanStateBytes = 64 << 10
const MaxDocToolTreeStepObjectBytes = 128 << 10
const maxDocToolTreeFrameBytes = 2048
const docToolTreeStructuralReadBytes = 128
const docToolTreeLexicalReadBytes = 4096

type docToolTreeFrame struct {
	Version     int             `json:"version"`
	Source      ContentHash     `json:"source"`
	Node        DocToolTreeNode `json:"node"`
	ParentFrame ContentHash     `json:"parent_frame,omitempty"`
	KeyFrom     int             `json:"key_from,omitempty"`
	PendingKey  *DocFindSpan    `json:"pending_key,omitempty"`
}

func (frame docToolTreeFrame) validate(source DocToolTreeSource, identity ContentHash) error {
	hashedParent := frame.Node.Depth > 0 && (frame.Node.Type == DocToolValueObject || frame.Node.Type == DocToolValueArray)
	if frame.Version != 1 || frame.Source != identity || frame.Node.validatePosition(source) != nil || frame.Node.Value.To != 0 || !hashedParent && frame.ParentFrame != "" || hashedParent && ValidateContentHash(frame.ParentFrame) != nil {
		return ErrIntegrity
	}
	if frame.KeyFrom != 0 && (frame.Node.Type != DocToolValueObject || frame.KeyFrom <= frame.Node.Value.From || frame.KeyFrom >= source.Root.To || frame.PendingKey != nil) {
		return ErrIntegrity
	}
	if key := frame.PendingKey; key != nil && (frame.Node.Type != DocToolValueObject || key.From <= frame.Node.Value.From || key.To <= key.From || key.To > source.Root.To || key.To-key.From < 2) {
		return ErrIntegrity
	}
	return nil
}

func (frame docToolTreeFrame) validateSuspended(source DocToolTreeSource, identity ContentHash) error {
	if frame.validate(source, identity) != nil || frame.KeyFrom != 0 || frame.PendingKey != nil || frame.Node.Children == 0 || frame.Node.Type != DocToolValueObject && frame.Node.Type != DocToolValueArray {
		return ErrIntegrity
	}
	return nil
}

func (frame docToolTreeFrame) encode(source DocToolTreeSource, identity ContentHash) ([]byte, error) {
	if frame.validateSuspended(source, identity) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(frame)
	if err != nil || len(raw) > maxDocToolTreeFrameBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func decodeDocToolTreeFrame(source DocToolTreeSource, identity, expected ContentHash, raw []byte) (docToolTreeFrame, error) {
	var frame docToolTreeFrame
	if len(raw) == 0 || len(raw) > maxDocToolTreeFrameBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &frame) != nil {
		return docToolTreeFrame{}, ErrIntegrity
	}
	canonical, err := frame.encode(source, identity)
	if err != nil || !bytes.Equal(canonical, raw) {
		return docToolTreeFrame{}, ErrIntegrity
	}
	return frame, nil
}

type DocToolTreeScanState struct {
	Version      int                    `json:"version"`
	Source       ContentHash            `json:"source"`
	Next         int                    `json:"next"`
	NodeCount    int                    `json:"node_count"`
	Complete     bool                   `json:"complete"`
	Syntax       DocFieldDiscoveryState `json:"syntax"`
	Current      *docToolTreeFrame      `json:"current,omitempty"`
	InlineParent *docToolTreeFrame      `json:"inline_parent,omitempty"`
}

func NewDocToolTreeScanState(source DocToolTreeSource) (DocToolTreeScanState, error) {
	identity, err := source.Hash()
	if err != nil {
		return DocToolTreeScanState{}, err
	}
	syntax := newDocBlockFieldParser(source.EventHash, source.EventBytes, source.Root.From)
	syntax.Started, syntax.Stack = true, []byte{docFieldArrayFirst}
	return DocToolTreeScanState{Version: 1, Source: identity, Next: source.Root.From, Syntax: syntax}, nil
}

func (state DocToolTreeScanState) Done() bool { return state.Complete }

func (state DocToolTreeScanState) Validate(source DocToolTreeSource) error {
	identity, err := source.Hash()
	if err != nil || state.Version != 1 || state.Source != identity || state.Next < source.Root.From || state.Next > source.Root.To || state.NodeCount < 0 || state.NodeCount > source.Root.To-source.Root.From || state.Done() != (state.Next == source.Root.To) {
		return ErrIntegrity
	}
	maximumNodes := (state.Next - source.Root.From + 1) / 2
	if source.Type != DocToolValueObject && source.Type != DocToolValueArray {
		maximumNodes = 1
		if !state.Done() {
			maximumNodes = 0
		}
	}
	if state.NodeCount > maximumNodes {
		return ErrIntegrity
	}
	syntax := state.Syntax
	if syntax.EventHash != source.EventHash || syntax.EventBytes != source.EventBytes || syntax.Next != state.Next || !syntax.Started || syntax.RootDone || syntax.Done || len(syntax.Stack) == 0 || len(syntax.HashState) != 0 || syntax.Digest != "" || syntax.Field != 0 || syntax.Kind != "" || syntax.Summary != nil || syntax.Blocks != nil || syntax.Output != nil || syntax.KindToken != nil || syntax.ToolName != nil || syntax.InvalidSummary || syntax.InvalidBlocks || syntax.InvalidToolName || syntax.OutputNonSpace || syntax.ToolInputType != 0 || syntax.ToolOutputType != 0 || syntax.StringPurpose != 0 || syntax.StringMask != 0 || syntax.StringRunes != 0 {
		return ErrIntegrity
	}
	syntax.Stack = bytes.Clone(syntax.Stack)
	switch syntax.Stack[0] {
	case docFieldArrayFirst, docFieldArrayValue:
		syntax.Stack[0] = docFieldObjectValue
	case docFieldArrayAfter:
		syntax.Stack[0] = docFieldObjectAfter
	default:
		return ErrIntegrity
	}
	if syntax.validateSyntax(docFieldSchemaBlock) != nil {
		return ErrIntegrity
	}
	if state.Current == nil {
		if state.InlineParent != nil {
			return ErrIntegrity
		}
		if state.Done() {
			if state.NodeCount == 0 || len(state.Syntax.Stack) != 1 || state.Syntax.Stack[0] != docFieldArrayAfter || syntax.Token != 0 || syntax.ValueFrom != 0 || syntax.ValueType != 0 {
				return ErrIntegrity
			}
			return nil
		}
		initial, err := NewDocToolTreeScanState(source)
		actual, actualErr := canonicalJSON(state)
		expected, expectedErr := canonicalJSON(initial)
		if err != nil || actualErr != nil || expectedErr != nil || !bytes.Equal(actual, expected) {
			return ErrIntegrity
		}
		return nil
	}
	frame := state.Current
	if state.Done() || frame.validate(source, identity) != nil || frame.Node.Value.From >= state.Next || state.Syntax.Stack[0] != docFieldArrayFirst || syntax.ValueFrom != source.Root.From || docToolTreeType(syntax.ValueType) != source.Type || frame.Node.Children > state.Next-frame.Node.Value.From-1 || frame.Node.Children > state.NodeCount || frame.Node.Ordinal > state.NodeCount {
		return ErrIntegrity
	}
	container := frame.Node.Type == DocToolValueObject || frame.Node.Type == DocToolValueArray
	if (state.InlineParent != nil) != (!container && frame.Node.Depth > 0) {
		return ErrIntegrity
	}
	if state.InlineParent != nil && (state.InlineParent.validateSuspended(source, identity) != nil || state.InlineParent.Node.Children > state.NodeCount+1 || validateDocToolTreeParent(state.Syntax, *state.InlineParent, *frame, false) != nil) {
		return ErrIntegrity
	}
	stackDepth := frame.Node.Depth + 1
	if container {
		stackDepth++
	}
	if len(state.Syntax.Stack) != stackDepth {
		return ErrIntegrity
	}
	top := state.Syntax.Stack[stackDepth-1]
	if container {
		if frame.Node.Type == DocToolValueObject && (top < docFieldObjectFirst || top > docFieldObjectAfter) || frame.Node.Type == DocToolValueArray && (top < docFieldArrayFirst || top > docFieldArrayAfter) || syntax.Token != 0 && syntax.Token != docFieldTokenKey {
			return ErrIntegrity
		}
		if syntax.Token == docFieldTokenKey {
			if frame.KeyFrom <= frame.Node.Value.From || frame.KeyFrom >= state.Next || frame.PendingKey != nil {
				return ErrIntegrity
			}
		} else if frame.KeyFrom != 0 {
			return ErrIntegrity
		}
		if (frame.PendingKey != nil) != (top == docFieldObjectColon || top == docFieldObjectValue) || frame.PendingKey != nil && frame.PendingKey.To > state.Next {
			return ErrIntegrity
		}
		if (top == docFieldObjectFirst || top == docFieldArrayFirst) && frame.Node.Children != 0 || (top == docFieldObjectKey || top == docFieldObjectAfter || top == docFieldArrayValue || top == docFieldArrayAfter) && frame.Node.Children == 0 {
			return ErrIntegrity
		}
	} else {
		if frame.KeyFrom != 0 || frame.PendingKey != nil || syntax.Token == 0 || syntax.Token == docFieldTokenKey || !docToolTreeTokenType(syntax.Token, frame.Node.Type) {
			return ErrIntegrity
		}
		if syntax.Token >= docFieldTokenNull && state.Next-frame.Node.Value.From != syntax.Literal || syntax.Token == docFieldTokenNumber && syntax.Number == 1 && state.Next-frame.Node.Value.From != 1 {
			return ErrIntegrity
		}
	}
	return nil
}

func docToolTreeTokenType(token byte, valueType DocToolValueType) bool {
	switch token {
	case docFieldTokenString:
		return valueType == DocToolValueString
	case docFieldTokenNumber:
		return valueType == DocToolValueNumber
	case docFieldTokenNull:
		return valueType == DocToolValueNull
	case docFieldTokenTrue:
		return valueType == DocToolValueTrue
	case docFieldTokenFalse:
		return valueType == DocToolValueFalse
	default:
		return false
	}
}

func (state DocToolTreeScanState) Encode(source DocToolTreeSource) ([]byte, error) {
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocToolTreeScanStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolTreeScanState(source DocToolTreeSource, expected ContentHash, raw []byte) (DocToolTreeScanState, error) {
	var state DocToolTreeScanState
	if len(raw) == 0 || len(raw) > MaxDocToolTreeScanStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocToolTreeScanState{}, ErrIntegrity
	}
	state.Syntax.schema = docFieldSchemaBlock
	canonical, err := state.Encode(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolTreeScanState{}, ErrIntegrity
	}
	return state, nil
}

type DocToolTreeScanStep struct {
	source     DocToolTreeSource
	prior      ContentHash
	state      DocToolTreeScanState
	objects    []DocRangeObject
	nodes      []DocToolTreeNode
	checkpoint []byte
	readBytes  int
}

func (step DocToolTreeScanStep) Source() DocToolTreeSource { return step.source }
func (step DocToolTreeScanStep) Prior() ContentHash        { return step.prior }
func (step DocToolTreeScanStep) ReadBytes() int            { return step.readBytes }

func (step DocToolTreeScanStep) State() DocToolTreeScanState {
	state := step.state
	state.Syntax.Stack = bytes.Clone(state.Syntax.Stack)
	state.Current = cloneDocToolTreeFrame(state.Current)
	state.InlineParent = cloneDocToolTreeFrame(state.InlineParent)
	return state
}

func cloneDocToolTreeFrame(frame *docToolTreeFrame) *docToolTreeFrame {
	if frame == nil {
		return nil
	}
	cloned := *frame
	cloned.Node.Key = cloneDocToolFieldSpan(frame.Node.Key)
	cloned.PendingKey = cloneDocToolFieldSpan(frame.PendingKey)
	return &cloned
}

func (step DocToolTreeScanStep) Objects() []DocRangeObject {
	if len(step.objects) == 0 {
		return nil
	}
	objects := make([]DocRangeObject, len(step.objects))
	for index, object := range step.objects {
		objects[index] = DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)}
	}
	return objects
}

func (step DocToolTreeScanStep) Nodes() []DocToolTreeNode {
	if len(step.nodes) == 0 {
		return nil
	}
	nodes := make([]DocToolTreeNode, len(step.nodes))
	for index, node := range step.nodes {
		node.Key = cloneDocToolFieldSpan(node.Key)
		nodes[index] = node
	}
	return nodes
}

func (step DocToolTreeScanStep) Checkpoint() (ContentHash, []byte) {
	if len(step.checkpoint) == 0 {
		return "", nil
	}
	return HashContent(step.checkpoint), bytes.Clone(step.checkpoint)
}

func docToolTreeType(character byte) DocToolValueType {
	if character == '-' || character >= '0' && character <= '9' {
		return DocToolValueNumber
	}
	return DocToolValueType(character)
}

type docToolTreeScanner struct {
	ctx     context.Context
	source  DocToolTreeSource
	state   DocToolTreeScanState
	load    func(context.Context, ContentHash) ([]byte, error)
	objects []DocRangeObject
	nodes   []DocToolTreeNode
	bytes   int
}

func (scanner *docToolTreeScanner) refillBytes() int {
	actions := min(DocToolTreeStepNodes-len(scanner.nodes), DocToolTreeStepFrames-len(scanner.objects))
	length := min(docToolTreeStructuralReadBytes, max(1, 2*actions+1))
	progress := 0
	if scanner.state.Current != nil {
		switch scanner.state.Syntax.Token {
		case docFieldTokenKey:
			progress = scanner.state.Next - scanner.state.Current.KeyFrom
		case docFieldTokenString, docFieldTokenNumber:
			progress = scanner.state.Next - scanner.state.Current.Node.Value.From
		}
	}
	if progress >= 32 {
		length = min(docToolTreeLexicalReadBytes, max(length, progress))
	}
	return length
}

func (scanner *docToolTreeScanner) parent(child docToolTreeFrame, closed bool) (*docToolTreeFrame, error) {
	if child.Node.Depth > 0 && child.Node.Type != DocToolValueObject && child.Node.Type != DocToolValueArray {
		parent := scanner.state.InlineParent
		if child.ParentFrame != "" || parent == nil || parent.validateSuspended(scanner.source, scanner.state.Source) != nil || validateDocToolTreeParent(scanner.state.Syntax, *parent, child, closed) != nil {
			return nil, ErrIntegrity
		}
		return parent, nil
	}
	if child.ParentFrame == "" {
		return nil, nil
	}
	if err := scanner.ctx.Err(); err != nil {
		return nil, err
	}
	var raw []byte
	for _, object := range scanner.objects {
		if object.Hash == child.ParentFrame {
			raw = object.Bytes
			break
		}
	}
	if raw == nil {
		if scanner.load == nil {
			return nil, ErrIntegrity
		}
		var err error
		raw, err = scanner.load(scanner.ctx, child.ParentFrame)
		if err != nil {
			return nil, err
		}
	}
	if err := scanner.ctx.Err(); err != nil {
		return nil, err
	}
	parent, err := decodeDocToolTreeFrame(scanner.source, scanner.state.Source, child.ParentFrame, raw)
	if err != nil || validateDocToolTreeParent(scanner.state.Syntax, parent, child, closed) != nil {
		return nil, ErrIntegrity
	}
	return &parent, nil
}

func validateDocToolTreeParent(syntax DocFieldDiscoveryState, parent, child docToolTreeFrame, closed bool) error {
	if parent.Node.Value.From != child.Node.Parent || parent.Node.Depth+1 != child.Node.Depth || parent.Node.Children != child.Node.Ordinal+1 || (child.Node.Key != nil) != (parent.Node.Type == DocToolValueObject) || parent.Node.Value.From >= child.Node.Value.From {
		return ErrIntegrity
	}
	slot := parent.Node.Depth + 1
	if slot >= len(syntax.Stack) {
		return ErrIntegrity
	}
	waiting := docFieldObjectValue
	if parent.Node.Type == DocToolValueArray {
		waiting = docFieldArrayValue
		if parent.Node.Children == 1 {
			waiting = docFieldArrayFirst
		}
	}
	if closed {
		waiting = docFieldObjectAfter
		if parent.Node.Type == DocToolValueArray {
			waiting = docFieldArrayAfter
		}
	}
	if syntax.Stack[slot] != waiting {
		return ErrIntegrity
	}
	return nil
}

func (scanner *docToolTreeScanner) start(character byte) error {
	node := DocToolTreeNode{Value: DocFindSpan{From: scanner.state.Next}, Type: docToolTreeType(character), Parent: -1}
	parentHash := ContentHash("")
	if parent := scanner.state.Current; parent != nil {
		node.Parent, node.Ordinal, node.Depth = parent.Node.Value.From, parent.Node.Children, parent.Node.Depth+1
		if node.Depth > docToolDisplayMaxDepth {
			return ErrDocFindDisplayUnavailable
		}
		node.Key = cloneDocToolFieldSpan(parent.PendingKey)
		if (node.Key != nil) != (parent.Node.Type == DocToolValueObject) {
			return ErrIntegrity
		}
		parent.Node.Children++
		parent.PendingKey = nil
		if node.Type == DocToolValueObject || node.Type == DocToolValueArray {
			raw, err := parent.encode(scanner.source, scanner.state.Source)
			if err != nil {
				return err
			}
			parentHash = HashContent(raw)
			scanner.objects = append(scanner.objects, DocRangeObject{Hash: parentHash, Bytes: raw})
			scanner.bytes += len(raw)
		} else {
			scanner.state.InlineParent = parent
		}
	}
	if node.validatePosition(scanner.source) != nil {
		return ErrIntegrity
	}
	scanner.state.Current = &docToolTreeFrame{Version: 1, Source: scanner.state.Source, Node: node, ParentFrame: parentHash}
	return nil
}

func (scanner *docToolTreeScanner) finish(end int, parent *docToolTreeFrame) error {
	frame := scanner.state.Current
	if frame == nil {
		return ErrIntegrity
	}
	node := frame.Node
	node.Value.To = end
	if node.Validate(scanner.source) != nil {
		return ErrIntegrity
	}
	if (node.Depth > 0) != (parent != nil) {
		return ErrIntegrity
	}
	scanner.nodes = append(scanner.nodes, node)
	scanner.state.NodeCount++
	scanner.state.Current = parent
	scanner.state.InlineParent = nil
	if node.Depth == 0 {
		if end != scanner.source.Root.To {
			return ErrIntegrity
		}
		scanner.state.Complete = true
	}
	return nil
}

func (scanner *docToolTreeScanner) accept(character byte, synthetic bool) (bool, error) {
	syntax := &scanner.state.Syntax
	token, stackDepth := syntax.Token, len(syntax.Stack)
	top := syntax.Stack[stackDepth-1]
	starting := token == 0 && !docFieldSpace(character) && (top == docFieldObjectValue || top == docFieldArrayFirst || top == docFieldArrayValue) && !(top == docFieldArrayFirst && character == ']')
	if scanner.state.Current == nil && (!starting || scanner.state.Next != scanner.source.Root.From) {
		return false, ErrIntegrity
	}
	if starting {
		if synthetic {
			return false, ErrIntegrity
		}
		if err := scanner.start(character); err != nil {
			return false, err
		}
	}
	keyStart := token == 0 && character == '"' && (top == docFieldObjectFirst || top == docFieldObjectKey)
	if keyStart {
		scanner.state.Current.KeyFrom = scanner.state.Next
	}
	consumed, err := syntax.accept(character)
	if err != nil {
		return false, err
	}
	if token == docFieldTokenKey && syntax.Token == 0 {
		frame := scanner.state.Current
		frame.PendingKey = &DocFindSpan{From: frame.KeyFrom, To: scanner.state.Next + 1}
		frame.KeyFrom = 0
	}
	completed := token != 0 && token != docFieldTokenKey && syntax.Token == 0 || len(syntax.Stack) < stackDepth
	if completed {
		parent, err := scanner.parent(*scanner.state.Current, true)
		if err != nil {
			return false, err
		}
		end := scanner.state.Next
		if consumed {
			end++
		}
		if err := scanner.finish(end, parent); err != nil {
			return false, err
		}
	}
	if consumed && !synthetic {
		scanner.state.Next++
		syntax.Next++
	}
	return consumed, nil
}

func StepDocToolTreeScan(ctx context.Context, source DocToolTreeSource, prior DocToolTreeScanState, loadRaw func(context.Context, int, int) ([]byte, error), loadObject func(context.Context, ContentHash) ([]byte, error)) (result DocToolTreeScanStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolTreeScanStep{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if prior.Validate(source) != nil || prior.Done() || loadRaw == nil {
		return result, ErrIntegrity
	}
	if err := ValidateDocToolTreeCheckpoint(ctx, source, prior, loadObject); err != nil {
		return result, err
	}
	encoded, err := prior.Encode(source)
	if err != nil {
		return result, err
	}
	state, err := DecodeDocToolTreeScanState(source, HashContent(encoded), encoded)
	if err != nil {
		return result, err
	}
	scanner := docToolTreeScanner{ctx: ctx, source: source, state: state, load: loadObject}
	readBytes := 0
	for !scanner.state.Done() && readBytes < DocToolTreeStepSourceBytes && len(scanner.nodes) < DocToolTreeStepNodes && len(scanner.objects) < DocToolTreeStepFrames {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if scanner.state.Next == source.Root.To {
			if scanner.state.Syntax.Token != docFieldTokenNumber {
				return result, ErrIntegrity
			}
			consumed, err := scanner.accept(' ', true)
			if err != nil || consumed || !scanner.state.Done() {
				if err != nil {
					return result, err
				}
				return result, ErrIntegrity
			}
			break
		}
		length := min(scanner.refillBytes(), source.Root.To-scanner.state.Next, DocToolTreeStepSourceBytes-readBytes)
		raw, err := loadRaw(ctx, scanner.state.Next, length)
		if err != nil {
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if len(raw) != length {
			return result, ErrIntegrity
		}
		readBytes += length
		for position := 0; position < len(raw) && !scanner.state.Done() && len(scanner.nodes) < DocToolTreeStepNodes && len(scanner.objects) < DocToolTreeStepFrames; {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			consumed, err := scanner.accept(raw[position], false)
			if err != nil {
				return result, err
			}
			if consumed {
				position++
			}
		}
	}
	if !scanner.state.Done() && scanner.state.Next == source.Root.To {
		if scanner.state.Syntax.Token != docFieldTokenNumber || len(scanner.nodes) >= DocToolTreeStepNodes {
			return result, ErrIntegrity
		}
		consumed, err := scanner.accept(' ', true)
		if err != nil || consumed || !scanner.state.Done() {
			if err != nil {
				return result, err
			}
			return result, ErrIntegrity
		}
	}
	checkpoint, err := scanner.state.Encode(source)
	if err != nil || scanner.state.Next < prior.Next || scanner.state.Next == prior.Next && scanner.state.NodeCount <= prior.NodeCount || len(scanner.objects) > DocToolTreeStepObjects || scanner.bytes > MaxDocToolTreeStepObjectBytes {
		return result, ErrIntegrity
	}
	return DocToolTreeScanStep{source: source, prior: HashContent(encoded), state: scanner.state, objects: scanner.objects, nodes: scanner.nodes, checkpoint: checkpoint, readBytes: readBytes}, nil
}

func ValidateDocToolTreeCheckpoint(ctx context.Context, source DocToolTreeSource, state DocToolTreeScanState, load func(context.Context, ContentHash) ([]byte, error)) (failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.Validate(source) != nil {
		return ErrIntegrity
	}
	if err := validateDocToolTreeEvidence(ctx, source, load); err != nil {
		return err
	}
	if state.Current != nil {
		scanner := docToolTreeScanner{ctx: ctx, source: source, state: state, load: load}
		frame := state.Current
		if state.InlineParent != nil {
			frame = state.InlineParent
		}
		if _, err := scanner.parent(*frame, false); err != nil {
			return err
		}
	}
	return nil
}

func FinishDocToolTreeScan(ctx context.Context, source DocToolTreeSource, state DocToolTreeScanState, load func(context.Context, ContentHash) ([]byte, error)) (result DocToolTreeScanState, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolTreeScanState{}
		}
	}()
	if err := ValidateDocToolTreeCheckpoint(ctx, source, state, load); err != nil {
		return result, err
	}
	if !state.Done() {
		return result, ErrIntegrity
	}
	raw, err := state.Encode(source)
	if err != nil {
		return result, err
	}
	return DecodeDocToolTreeScanState(source, HashContent(raw), raw)
}
