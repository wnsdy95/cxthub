package domain

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"unsafe"
)

const legacyNativeAnyCheckpointScratch = 8 * LegacyCanonicalEncodingBytes
const legacyNativeAnyCheckpointRoots = 5
const legacyNativeAnyCheckpointDiagnosticBytes = 4096

type legacyNativeAnyCheckpointTask[Value any] interface {
	step(context.Context) (bool, error)
	take(context.Context) (Value, error)
	retainedBytes() (int64, error)
	close()
}

type legacyNativeAnyCheckpointNumberCapture interface {
	legacyNativeAnyCheckpointTask[legacyNativeCheckpointRef]
	bindSource(context.Context, LegacyByteSource) error
}

type legacyNativeAnyCheckpointNumberRecovery interface {
	legacyNativeAnyCheckpointTask[*legacyNativeNumberValue]
	source() LegacyByteSource
}

type legacyNativeAnyCheckpointRoot struct {
	Slot  uint8                       `json:"s"`
	Value legacyNativeCheckpointValue `json:"v"`
}

type legacyNativeAnyCheckpointFrame struct {
	Index uint32                      `json:"i"`
	Phase uint8                       `json:"p"`
	Owner legacyNativeCheckpointValue `json:"o"`
	Key   legacyNativeCheckpointValue `json:"k"`
}

type legacyNativeAnyCheckpointError struct {
	Value  legacyNativeCheckpointScalar `json:"v"`
	Type   string                       `json:"t"`
	Offset int64                        `json:"o"`
	Struct string                       `json:"s"`
	Field  string                       `json:"f"`
}

type legacyNativeAnyCheckpointControl struct {
	Version          uint8                           `json:"v"`
	Binding          ContentHash                     `json:"b"`
	SourceBytes      int                             `json:"n"`
	From             int                             `json:"from"`
	End              int                             `json:"end"`
	Position         int                             `json:"position"`
	ParentDepth      int                             `json:"parents"`
	Depth            int                             `json:"depth"`
	FieldLimit       int                             `json:"limit"`
	TokenStart       int                             `json:"token_start"`
	TokenEnd         int                             `json:"token_end"`
	TokenFirst       byte                            `json:"token_first"`
	KeyToken         bool                            `json:"key"`
	Scanning         bool                            `json:"scanning"`
	Applying         bool                            `json:"applying"`
	Pending          bool                            `json:"pending"`
	RootReserved     bool                            `json:"reserved"`
	Finished         bool                            `json:"finished"`
	ErrorContextSet  bool                            `json:"context"`
	RawCapacity      int                             `json:"raw_capacity"`
	OpeningRemaining int                             `json:"opening"`
	ClosingRemaining int                             `json:"closing"`
	ClosingByte      byte                            `json:"closing_byte"`
	Roots            legacyNativeCheckpointTree      `json:"roots"`
	Frames           legacyNativeCheckpointTree      `json:"frames"`
	Scanner          legacyNativeCheckpointRef       `json:"scanner"`
	HasScalar        bool                            `json:"has_scalar"`
	Scalar           legacyNativeCheckpointScalar    `json:"scalar"`
	Number           legacyNativeCheckpointRef       `json:"number"`
	Error            *legacyNativeAnyCheckpointError `json:"error"`
}

func legacyNativeAnyCheckpointControlFor(parser *legacyNativeAny, binding ContentHash, sourceBytes int) legacyNativeAnyCheckpointControl {
	return legacyNativeAnyCheckpointControl{
		Version: legacyNativeCheckpointVersion, Binding: binding, SourceBytes: sourceBytes,
		From: parser.from, End: parser.end, Position: parser.position, ParentDepth: parser.parentDepth,
		Depth: parser.depth, FieldLimit: parser.fieldLimit, TokenStart: parser.tokenStart, TokenEnd: parser.tokenEnd,
		TokenFirst: parser.tokenFirst, KeyToken: parser.keyToken, Scanning: parser.scanning, Applying: parser.applying,
		Pending: parser.pending, RootReserved: parser.rootReserved, Finished: parser.finished,
		ErrorContextSet: parser.errorContextSet, RawCapacity: cap(parser.raw), HasScalar: parser.scalar != nil,
		OpeningRemaining: parser.openingRemaining, ClosingRemaining: parser.closingRemaining, ClosingByte: parser.closingByte,
	}
}

func (state legacyNativeAnyCheckpointControl) validate(binding ContentHash, sourceBytes int, complete bool) error {
	if state.Version != legacyNativeCheckpointVersion || ValidateContentHash(binding) != nil ||
		state.Binding != binding || sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes || state.SourceBytes != sourceBytes ||
		state.From < 0 || state.End <= state.From || state.End > sourceBytes || state.Position < state.From || state.Position > state.End ||
		state.ParentDepth < 0 || state.ParentDepth >= 10000 || state.Depth < 0 || state.Depth > 10000-state.ParentDepth || state.FieldLimit <= 0 ||
		state.RawCapacity < 0 || state.RawCapacity > LegacyCanonicalEncodingBytes || state.TokenStart < 0 || state.TokenStart > state.End ||
		state.TokenEnd < 0 || state.TokenEnd > state.End || state.OpeningRemaining < 0 || state.OpeningRemaining > 10000-state.ParentDepth-state.Depth ||
		state.ClosingRemaining < 0 || state.ClosingRemaining > state.Depth ||
		state.OpeningRemaining > state.End-state.Position || state.ClosingRemaining > state.End-state.Position {
		return ErrIntegrity
	}
	if state.TokenFirst != 0 && state.TokenFirst != '"' && state.TokenFirst != 't' && state.TokenFirst != 'f' &&
		state.TokenFirst != 'n' && state.TokenFirst != '-' && (state.TokenFirst < '0' || state.TokenFirst > '9') {
		return ErrIntegrity
	}
	active := state.Scanning || state.Applying || state.Pending
	if state.Scanning && state.Applying || state.Pending && (state.Scanning || state.Applying) ||
		state.Depth == 0 && active || state.KeyToken && (!active || state.TokenFirst != '"') ||
		state.HasScalar && (!state.Applying || state.TokenFirst != '"') ||
		state.Finished && (state.Depth != 0 || active || !state.RootReserved || state.OpeningRemaining != 0 || state.ClosingRemaining != 0) ||
		state.OpeningRemaining > 0 && (active || state.ClosingRemaining != 0) ||
		state.ClosingRemaining > 0 && (state.Scanning || state.Applying || state.KeyToken || state.ClosingByte != ']' && state.ClosingByte != '}') ||
		state.ClosingRemaining == 0 && state.ClosingByte != 0 {
		return ErrIntegrity
	}
	if state.Scanning || state.Applying {
		if state.TokenFirst == 0 || state.TokenStart < state.From || state.TokenStart >= state.End || state.TokenStart > state.Position ||
			state.Applying && (state.TokenEnd <= state.TokenStart || state.TokenEnd != state.Position) {
			return ErrIntegrity
		}
	}
	if complete {
		if !state.Roots.valid() || state.Roots.Count != legacyNativeAnyCheckpointRoots || !state.Frames.valid() ||
			state.Frames.Count != uint64(state.Depth) || !state.Scanner.valid() ||
			state.Number != (legacyNativeCheckpointRef{}) && (!state.Number.valid() || !state.Applying || state.HasScalar ||
				state.TokenFirst != '-' && (state.TokenFirst < '0' || state.TokenFirst > '9') || state.TokenEnd-state.TokenStart <= LegacyCanonicalEncodingBytes) {
			return ErrIntegrity
		}
		if state.HasScalar {
			if err := state.Scalar.State.validate(sourceBytes); err != nil {
				return err
			}
			if state.Scalar.State.Position < state.TokenStart+1 || state.Scalar.State.End != state.TokenEnd-1 ||
				state.Scalar.State.Maximum != 3*(state.TokenEnd-state.TokenStart-2) || state.Scalar.Text.Bytes != state.Scalar.State.Bytes {
				return ErrIntegrity
			}
		} else if state.Scalar != (legacyNativeCheckpointScalar{}) {
			return ErrIntegrity
		}
	}
	if state.Error == nil {
		if complete && state.ErrorContextSet {
			return ErrIntegrity
		}
	} else {
		semantic := state.Error
		if semantic.Type != "float64" || semantic.Offset <= 0 || semantic.Offset > 2*int64(sourceBytes)+1 ||
			len(semantic.Struct) > legacyNativeAnyCheckpointDiagnosticBytes || len(semantic.Field) > legacyNativeAnyCheckpointDiagnosticBytes {
			return ErrIntegrity
		}
		if complete {
			if err := semantic.Value.State.validate(sourceBytes); err != nil {
				return err
			}
			text := semantic.Value.State
			if text.Position != 0 || text.End != 0 || !text.Present || text.Expanded || text.Bytes != text.Maximum ||
				text.Capacity != text.Bytes || semantic.Value.Text.Bytes != text.Bytes || text.Bytes < len("number ") {
				return ErrIntegrity
			}
		}
	}
	return nil
}

func legacyNativeAnyCheckpointOwnerValue(owner legacyNativeAnyValueOwner) (legacyNativeAnyValue, error) {
	if owner.object == nil && owner.array == nil {
		if err := owner.validate(); err != nil {
			return legacyNativeAnyValue{}, err
		}
		return legacyNativeAnyValue{}, nil
	}
	value := legacyNativeAnyValue{owner: owner}
	if owner.object != nil {
		value.value, value.heap, value.excess = owner.object.values(), owner.object.heap(), owner.object.backingExcess()
	} else {
		value.value, value.heap, value.excess = owner.array.values(), owner.array.heap(), owner.array.backingExcess()
	}
	return value, value.validateOwnership()
}

func legacyNativeAnyCheckpointContainer(object *legacyNativeAnyMap, array *legacyNativeAnyArray) (legacyNativeAnyValue, error) {
	if object != nil && array != nil {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	var retained docRangeHeaderHeapEstimate
	owner := legacyNativeAnyValueOwner{object: object, array: array}
	if object != nil {
		object.retained(&retained)
		owner.revision = object.revision
	}
	if array != nil {
		array.retained(&retained)
		owner.revision = array.revision
	}
	if retained.err != nil {
		return legacyNativeAnyValue{}, retained.err
	}
	owner.bytes = retained.bytes
	return legacyNativeAnyCheckpointOwnerValue(owner)
}

func legacyNativeAnyCheckpointString(text string, capacity int) (legacyNativeAnyValue, error) {
	if capacity < len(text) {
		return legacyNativeAnyValue{}, ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(text)))
	heap.allocation(int64(capacity), 1)
	value := legacyNativeAnyValue{value: text, capacity: capacity, heap: heap, excess: 2 * int64(capacity-len(text))}
	return value, value.validateOwnership()
}

func legacyNativeAnyCheckpointOwnerKey(owner legacyNativeAnyValueOwner) legacyNativeAnyValueOwner {
	return legacyNativeAnyValueOwner{object: owner.object, array: owner.array}
}

func legacyNativeAnyCheckpointSeenHeap(count int) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, 64)
	heap.allocation(int64(max(8, count)), 128)
	return heap
}

func legacyNativeAnyCheckpointCheckRoots(parser *legacyNativeAny) error {
	root := legacyNativeAnyValueOwner{object: parser.rootMap, array: parser.rootArray}
	if root.object != nil && root.array != nil || !parser.pending && parser.node.value != nil ||
		!parser.finished && parser.result.value != nil {
		return ErrIntegrity
	}
	if parser.finished {
		if root == (legacyNativeAnyValueOwner{}) || root != legacyNativeAnyCheckpointOwnerKey(parser.result.owner) {
			return ErrIntegrity
		}
	} else if parser.depth == 0 {
		if parser.rootArray != nil || parser.rootReserved != (parser.rootMap != nil) {
			return ErrIntegrity
		}
	} else if parser.rootReserved || root == (legacyNativeAnyValueOwner{}) {
		return ErrIntegrity
	}
	if parser.scalar != nil && (!parser.applying || parser.tokenFirst != '"') ||
		parser.number != nil && (!parser.applying || parser.tokenEnd-parser.tokenStart <= LegacyCanonicalEncodingBytes ||
			parser.tokenFirst != '-' && (parser.tokenFirst < '0' || parser.tokenFirst > '9') || parser.number.parser == nil ||
			parser.number.parser.from != parser.tokenStart || parser.number.parser.end != parser.tokenEnd) {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeAnyCheckpointSourceGeometry(source LegacyByteSource, sourceBytes int) error {
	partBytes := source.partBytes()
	if source.Bytes != sourceBytes || sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes || source.authority == 0 ||
		partBytes == 0 || source.Read == nil || len(source.Parts) != 1+(sourceBytes-1)/partBytes {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeAnyCheckpointSourcePart(source LegacyByteSource, ordinal int) error {
	if ordinal < 0 || ordinal >= len(source.Parts) {
		return ErrIntegrity
	}
	part := source.Parts[ordinal]
	if ValidateContentHash(part.Hash) != nil || part.Bytes != int64(min(source.partBytes(), source.Bytes-ordinal*source.partBytes())) {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeAnyCheckpointSourceHeap(source LegacyByteSource) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.include(source.checkpointReaderHeap)
	if source.Parts != nil {
		heap.allocation(int64(cap(source.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
		heap.allocation(int64(len(source.Parts)), 128)
	}
	return heap
}

func legacyNativeAnyCheckpointControlHeap(state legacyNativeAnyCheckpointControl) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	for _, hash := range []ContentHash{state.Binding, state.Roots.Ref.Hash, state.Frames.Ref.Hash, state.Scanner.Hash,
		state.Scalar.Text.Tree.Ref.Hash, state.Number.Hash} {
		heap.allocation(int64(len(hash)), 1)
	}
	if state.Error != nil {
		heap.allocation(int64(len(state.Error.Value.Text.Tree.Ref.Hash)), 1)
	}
	return heap
}

func legacyNativeAnyCheckpointDescriptorHeap(value legacyNativeCheckpointValue) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(int64(cap(value.Wire)), 1)
	heap.allocation(int64(len(value.Body.Hash)), 1)
	heap.allocation(int64(len(value.Text.Tree.Ref.Hash)), 1)
	return heap
}

func legacyNativeAnyCheckpointSameValue(first, second legacyNativeAnyValue) bool {
	if first.heap != second.heap || first.excess != second.excess || first.capacity != second.capacity || first.owner != second.owner ||
		reflect.TypeOf(first.value) != reflect.TypeOf(second.value) {
		return false
	}
	switch decoded := first.value.(type) {
	case nil:
		return true
	case string:
		other := second.value.(string)
		return len(decoded) == len(other) && unsafe.StringData(decoded) == unsafe.StringData(other)
	case float64:
		return math.Float64bits(decoded) == math.Float64bits(second.value.(float64))
	case bool:
		return decoded == second.value.(bool)
	case map[string]any:
		return reflect.ValueOf(decoded).Pointer() == reflect.ValueOf(second.value).Pointer()
	case []any:
		other := second.value.([]any)
		return len(decoded) == len(other) && cap(decoded) == cap(other) && reflect.ValueOf(decoded).Pointer() == reflect.ValueOf(other).Pointer()
	}
	return false
}

type legacyNativeAnyCheckpointFrameGuard struct {
	Source *legacyNativeAnyFrame
	Shape  legacyNativeAnyFrame
	Owner  legacyNativeAnyValueOwner
}

type legacyNativeAnyCheckpoint struct {
	sharedField      *legacyNativeObjectAnyCheckpoint
	sharedGraph      *legacyNativeCheckpointGraphIdentity
	parser           *legacyNativeAny
	scanner          *legacyJSONValueScanner
	shape            legacyNativeAny
	scannerShape     legacyJSONValueScanner
	scalarShape      legacyNativeScalar
	scalarLength     int
	scalarCapacity   int
	previousLength   int
	previousCapacity int
	numberShape      legacyNativeNumberValue
	numberTextShape  legacyNativeScalar
	numberTextLength int
	numberTextCap    int
	numberOldLength  int
	numberOldCap     int
	numberErrorShape json.UnmarshalTypeError
	floatShape       legacyNativeFloat64
	decimalShape     legacyNativeFloat64Decimal
	errorShape       json.UnmarshalTypeError
	state            legacyNativeAnyCheckpointControl
	store            legacyNativeCheckpointIO
	source           LegacyByteSource
	sourceValidated  int
	started          bool
	budget           int64
	phase            uint8
	rootIndex        int
	frameIndex       int
	framePart        uint8
	frame            *legacyNativeAnyFrame
	frameRecord      legacyNativeAnyCheckpointFrame
	frameLive        docRangeHeaderHeapEstimate
	seen             map[legacyNativeAnyValueOwner]struct{}
	guards           legacyCanonicalPages[legacyNativeAnyCheckpointFrameGuard]
	guardIndex       int
	roots            legacyNativeCheckpointListWriter[legacyNativeAnyCheckpointRoot]
	frames           legacyNativeCheckpointListWriter[legacyNativeAnyCheckpointFrame]
	graph            legacyNativeAnyCheckpointTask[legacyNativeCheckpointValue]
	scalar           legacyNativeAnyCheckpointTask[legacyNativeCheckpointScalar]
	number           legacyNativeAnyCheckpointNumberCapture
	errorText        legacyNativeAnyCheckpointTask[legacyNativeCheckpointText]
	startGraph       func(context.Context, legacyNativeAnyValue, int64) (legacyNativeAnyCheckpointTask[legacyNativeCheckpointValue], error)
	identityHeap     func() (int64, error)
	identityClose    func()
	root             legacyNativeCheckpointRef
	ready            bool
	closed           bool
	failure          error
}

func newLegacyNativeAnyCheckpoint(ctx context.Context, parser *legacyNativeAny, scanner *legacyJSONValueScanner, binding ContentHash, sourceBytes int, store legacyNativeCheckpointIO, heapBudget int64) (*legacyNativeAnyCheckpoint, error) {
	return newLegacyNativeAnyCheckpointInField(ctx, parser, scanner, binding, sourceBytes, store, heapBudget, nil)
}

func newLegacyNativeAnyCheckpointInField(ctx context.Context, parser *legacyNativeAny, scanner *legacyJSONValueScanner, binding ContentHash, sourceBytes int, store legacyNativeCheckpointIO, heapBudget int64, field *legacyNativeObjectAnyCheckpoint) (result *legacyNativeAnyCheckpoint, failure error) {
	var registered *legacyNativeAnyCheckpoint
	defer func() {
		if failure != nil && field != nil && field.child == registered {
			field.child = nil
		}
	}()
	if field != nil {
		parent, err := field.sharedCaptureRoot()
		if err != nil {
			return nil, err
		}
		if field.child != nil || field.snapshot.parser != parser || field.binding != binding || parent.source.Bytes != sourceBytes {
			return nil, ErrConflict
		}
		store = parent.store
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parser == nil || scanner == nil || store.Put == nil || heapBudget <= 0 {
		return nil, ErrIntegrity
	}
	state := legacyNativeAnyCheckpointControlFor(parser, binding, sourceBytes)
	if err := state.validate(binding, sourceBytes, false); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeAnyCheckpoint{})))
	planned.include(legacyNativeAnyCheckpointSeenHeap(0))
	planned.include(legacyNativeAnyCheckpointControlHeap(state))
	parser.retained(&planned)
	planned.include(docRangeHeaderHeapEstimate{bytes: legacyNativeAnyCheckpointScratch})
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	work := &legacyNativeAnyCheckpoint{sharedField: field, parser: parser, scanner: scanner, shape: *parser, scannerShape: *scanner,
		state: state, store: store, budget: heapBudget, frame: parser.frame, seen: make(map[legacyNativeAnyValueOwner]struct{})}
	if field != nil {
		registered, field.child, work.sharedGraph = work, work, field.identity
	}
	if parser.scalar != nil {
		work.scalarShape = *parser.scalar
		if parser.scalar.text != nil {
			work.scalarLength, work.scalarCapacity = parser.scalar.text.Len(), parser.scalar.text.Cap()
		}
		if parser.scalar.previous != nil {
			work.previousLength, work.previousCapacity = parser.scalar.previous.Len(), parser.scalar.previous.Cap()
		}
	}
	if parser.number != nil {
		work.numberShape = *parser.number
		if parser.number.errorText != nil {
			work.numberTextShape = *parser.number.errorText
			if parser.number.errorText.text != nil {
				work.numberTextLength, work.numberTextCap = parser.number.errorText.text.Len(), parser.number.errorText.text.Cap()
			}
			if parser.number.errorText.previous != nil {
				work.numberOldLength, work.numberOldCap = parser.number.errorText.previous.Len(), parser.number.errorText.previous.Cap()
			}
		}
		if parser.number.semantic != nil {
			work.numberErrorShape = *parser.number.semantic
		}
		if parser.number.parser == nil {
			work.close()
			return nil, ErrIntegrity
		}
		work.floatShape = *parser.number.parser
		if parser.number.parser.decimal != nil {
			work.decimalShape = *parser.number.parser.decimal
		}
	}
	if parser.semanticError != nil {
		if parser.semanticError.Type != reflect.TypeFor[float64]() {
			work.close()
			return nil, ErrIntegrity
		}
		work.errorShape = *parser.semanticError
		work.state.Error = &legacyNativeAnyCheckpointError{Type: "float64", Offset: parser.semanticError.Offset,
			Struct: parser.semanticError.Struct, Field: parser.semanticError.Field}
	}
	if err := work.state.validate(binding, sourceBytes, false); err != nil {
		work.close()
		return nil, err
	}
	if err := legacyNativeAnyCheckpointCheckScanner(work.state, scanner); err != nil {
		work.close()
		return nil, err
	}
	if err := legacyNativeAnyCheckpointCheckRoots(parser); err != nil {
		work.close()
		return nil, err
	}
	identity := work.sharedGraph
	if identity == nil {
		identity = newLegacyNativeCheckpointGraphIdentity()
	}
	work.startGraph = func(current context.Context, value legacyNativeAnyValue, budget int64) (legacyNativeAnyCheckpointTask[legacyNativeCheckpointValue], error) {
		return newLegacyNativeCheckpointGraphWriterInField(current, value, store, identity, budget, field, work.sharedGraphAny())
	}
	work.identityHeap = identity.retainedBytes
	if field == nil {
		work.identityClose = identity.close
	}
	if err := work.checkSource(); err != nil {
		work.close()
		return nil, err
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeAnyCheckpoint) bindSource(ctx context.Context, live LegacyByteSource) error {
	if work == nil {
		return ErrIntegrity
	}
	if work.failure != nil {
		return work.failure
	}
	if err := ctx.Err(); err != nil {
		_, failure := work.fail(err)
		return failure
	}
	if work.closed {
		return ErrIntegrity
	}
	if work.started || work.source.Read != nil {
		_, err := work.fail(ErrIntegrity)
		return err
	}
	if err := legacyNativeAnyCheckpointSourceGeometry(live, work.state.SourceBytes); err != nil {
		_, failure := work.fail(err)
		return failure
	}
	planned := legacyNativeAnyCheckpointSourceHeap(live)
	if planned.err != nil {
		_, failure := work.fail(planned.err)
		return failure
	}
	if err := work.admit(planned.bytes + legacyNativeAnyCheckpointScratch); err != nil {
		_, failure := work.fail(err)
		return failure
	}
	work.source = live
	return nil
}

func (work *legacyNativeAnyCheckpoint) checkSource() error {
	if work == nil || work.closed {
		return ErrConflict
	}
	if err := work.refreshSharedBudget(); err != nil {
		return err
	}
	parser := work.parser
	if parser == nil || work.scanner == nil || *work.scanner != work.scannerShape ||
		legacyNativeAnyCheckpointControlFor(parser, work.state.Binding, work.state.SourceBytes) !=
			legacyNativeAnyCheckpointControlFor(&work.shape, work.state.Binding, work.state.SourceBytes) ||
		parser.frame != work.shape.frame || parser.rootMap != work.shape.rootMap || parser.rootArray != work.shape.rootArray ||
		parser.visible != work.shape.visible || parser.scalar != work.shape.scalar || parser.number != work.shape.number ||
		parser.semanticError != work.shape.semanticError || parser.live != work.shape.live || len(parser.raw) != len(work.shape.raw) ||
		!legacyNativeAnyCheckpointSameValue(parser.node, work.shape.node) || !legacyNativeAnyCheckpointSameValue(parser.result, work.shape.result) {
		return ErrConflict
	}
	if parser.scalar != nil {
		if *parser.scalar != work.scalarShape || parser.scalar.text != nil &&
			(parser.scalar.text.Len() != work.scalarLength || parser.scalar.text.Cap() != work.scalarCapacity) ||
			parser.scalar.previous != nil && (parser.scalar.previous.Len() != work.previousLength || parser.scalar.previous.Cap() != work.previousCapacity) {
			return ErrConflict
		}
	}
	if parser.number != nil && (*parser.number != work.numberShape || *parser.number.parser != work.floatShape ||
		parser.number.parser.decimal != nil && *parser.number.parser.decimal != work.decimalShape) {
		return ErrConflict
	}
	if parser.number != nil {
		if math.Float64bits(parser.number.value) != math.Float64bits(work.numberShape.value) ||
			parser.number.semantic != nil && *parser.number.semantic != work.numberErrorShape {
			return ErrConflict
		}
		if text := parser.number.errorText; text != nil {
			if *text != work.numberTextShape || text.text != nil && (text.text.Len() != work.numberTextLength || text.text.Cap() != work.numberTextCap) ||
				text.previous != nil && (text.previous.Len() != work.numberOldLength || text.previous.Cap() != work.numberOldCap) {
				return ErrConflict
			}
		}
	}
	if parser.semanticError != nil && *parser.semanticError != work.errorShape {
		return ErrConflict
	}
	return nil
}

func (work *legacyNativeAnyCheckpoint) rootValue() (legacyNativeAnyValue, error) {
	switch work.rootIndex {
	case 0:
		return legacyNativeAnyCheckpointContainer(work.parser.rootMap, nil)
	case 1:
		return legacyNativeAnyCheckpointContainer(nil, work.parser.rootArray)
	case 2:
		return work.parser.node, work.parser.node.validateOwnership()
	case 3:
		return work.parser.result, work.parser.result.validateOwnership()
	case 4:
		return legacyNativeAnyCheckpointOwnerValue(work.parser.visible)
	}
	return legacyNativeAnyValue{}, ErrIntegrity
}

func (work *legacyNativeAnyCheckpoint) graphValue(ctx context.Context, value legacyNativeAnyValue) (legacyNativeCheckpointValue, bool, error) {
	if work.graph == nil {
		remaining, err := work.remaining()
		if err != nil {
			return legacyNativeCheckpointValue{}, false, err
		}
		shared, err := work.identityHeap()
		if err != nil {
			return legacyNativeCheckpointValue{}, false, err
		}
		work.graph, err = work.startGraph(ctx, value, remaining+shared)
		if err != nil {
			return legacyNativeCheckpointValue{}, false, err
		}
	}
	ready, err := work.graph.step(ctx)
	if err != nil || !ready {
		return legacyNativeCheckpointValue{}, false, err
	}
	saved, err := work.graph.take(ctx)
	work.graph.close()
	work.graph = nil
	return saved, err == nil, err
}

func (work *legacyNativeAnyCheckpoint) advance(ctx context.Context) error {
	switch work.phase {
	case 0:
		raw, err := work.scanner.checkpoint(ctx)
		if err != nil {
			return err
		}
		work.state.Scanner, err = legacyNativeCheckpointWrite(ctx, work.store, raw)
		if err != nil {
			return err
		}
		work.phase++
	case 1:
		if work.parser.scalar == nil {
			work.phase++
			return nil
		}
		if work.scalar == nil {
			current, err := newLegacyNativeCheckpointScalarWriter(ctx, work.parser.scalar, work.store)
			if err != nil {
				return err
			}
			work.scalar = current
		}
		ready, err := work.scalar.step(ctx)
		if err != nil || !ready {
			return err
		}
		work.state.Scalar, err = work.scalar.take(ctx)
		work.scalar.close()
		work.scalar = nil
		if err != nil {
			return err
		}
		work.phase++
	case 2:
		if work.parser.number == nil {
			work.phase++
			return nil
		}
		if work.number == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			current, err := newLegacyNativeCheckpointNumberWriter(ctx, work.parser.number, work.store, work.state.SourceBytes, remaining)
			if err != nil {
				return err
			}
			work.number = current
			if work.source.Read != nil {
				if err := work.number.bindSource(ctx, work.source); err != nil {
					return err
				}
			}
		}
		ready, err := work.number.step(ctx)
		if err != nil || !ready {
			return err
		}
		work.state.Number, err = work.number.take(ctx)
		work.number.close()
		work.number = nil
		if err != nil {
			return err
		}
		work.phase++
	case 3:
		if work.state.Error == nil {
			work.phase++
			return nil
		}
		if work.errorText == nil {
			text := work.errorShape.Value
			read := func(current context.Context, offset, length int) ([]byte, error) {
				if err := current.Err(); err != nil {
					return nil, err
				}
				if offset < 0 || length < 0 || length > LegacyCanonicalEncodingBytes || offset > len(text) || length > len(text)-offset {
					return nil, ErrIntegrity
				}
				return []byte(text[offset : offset+length]), nil
			}
			current, err := newLegacyNativeCheckpointTextWriter(ctx, len(text), read, work.store)
			if err != nil {
				return err
			}
			work.errorText = current
			work.state.Error.Value.State = legacyNativeScalarCheckpoint{Version: 1, Maximum: len(text), Bytes: len(text), Capacity: len(text), Present: true}
		}
		ready, err := work.errorText.step(ctx)
		if err != nil || !ready {
			return err
		}
		work.state.Error.Value.Text, err = work.errorText.take(ctx)
		work.errorText.close()
		work.errorText = nil
		if err != nil {
			return err
		}
		work.phase++
	case 4:
		if work.rootIndex == legacyNativeAnyCheckpointRoots {
			work.phase++
			return nil
		}
		value, err := work.rootValue()
		if err != nil {
			return err
		}
		saved, ready, err := work.graphValue(ctx, value)
		if err != nil || !ready {
			return err
		}
		if err := work.roots.append(ctx, work.store, legacyNativeAnyCheckpointRoot{Slot: uint8(work.rootIndex), Value: saved}); err != nil {
			return err
		}
		work.rootIndex++
	case 5:
		return work.captureFrame(ctx)
	case 6:
		var err error
		work.state.Roots, err = work.roots.finish(ctx, work.store)
		if err != nil {
			return err
		}
		work.roots.close()
		work.phase++
	case 7:
		var err error
		work.state.Frames, err = work.frames.finish(ctx, work.store)
		if err != nil {
			return err
		}
		work.frames.close()
		work.phase++
	case 8:
		if err := work.state.validate(work.state.Binding, work.state.SourceBytes, true); err != nil {
			return err
		}
		var err error
		work.root, err = legacyNativeCheckpointWriteJSON(ctx, work.store, work.state)
		if err != nil {
			return err
		}
		work.phase++
	case 9:
		if work.guardIndex == work.guards.length() {
			work.ready = true
			return nil
		}
		guard := work.guards.at(work.guardIndex)
		if guard == nil || guard.Source == nil || *guard.Source != guard.Shape {
			return ErrConflict
		}
		if err := guard.Owner.validate(); err != nil {
			return err
		}
		work.guardIndex++
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeAnyCheckpoint) captureFrame(ctx context.Context) error {
	if work.frame == nil {
		if work.frameIndex != work.state.Depth || work.frameLive != work.shape.live {
			return ErrIntegrity
		}
		work.phase++
		return nil
	}
	if work.frameIndex >= work.state.Depth {
		return ErrIntegrity
	}
	value, err := legacyNativeAnyCheckpointContainer(work.frame.object, work.frame.array)
	if err != nil || value.owner == (legacyNativeAnyValueOwner{}) {
		if err != nil {
			return err
		}
		return ErrIntegrity
	}
	if err := legacyNativeAnyCheckpointCheckFrame(work.state, work.frameIndex, work.frame, value.owner, work.shape.node.owner); err != nil {
		return err
	}
	if work.framePart == 0 {
		ownerKey := legacyNativeAnyCheckpointOwnerKey(value.owner)
		if _, found := work.seen[ownerKey]; found {
			return ErrIntegrity
		}
		rootKey := legacyNativeAnyValueOwner{object: work.shape.rootMap, array: work.shape.rootArray}
		if (work.frameIndex == work.state.Depth-1) != (ownerKey == rootKey) {
			return ErrIntegrity
		}
		estimate := work.frame.estimate()
		if estimate.err != nil || estimate.bytes != work.frame.retained {
			return ErrIntegrity
		}
		work.frameLive.include(estimate)
		if work.frameLive.err != nil {
			return work.frameLive.err
		}
		_, future, err := work.guards.appendRequirements()
		if err != nil {
			return err
		}
		var current docRangeHeaderHeapEstimate
		work.guards.retained(&current)
		if current.err != nil || future.bytes < current.bytes {
			return ErrIntegrity
		}
		if err := work.admit(future.bytes - current.bytes + 512 + legacyNativeAnyCheckpointScratch); err != nil {
			return err
		}
		if err := work.guards.append(legacyNativeAnyCheckpointFrameGuard{Source: work.frame, Shape: *work.frame, Owner: value.owner}); err != nil {
			return err
		}
		work.seen[ownerKey] = struct{}{}
		work.frameRecord = legacyNativeAnyCheckpointFrame{Index: uint32(work.frameIndex), Phase: work.frame.phase}
		work.framePart = 1
		return nil
	}
	if work.framePart == 1 {
		saved, ready, err := work.graphValue(ctx, value)
		if err != nil || !ready {
			return err
		}
		work.frameRecord.Owner, work.framePart = saved, 2
		return nil
	}
	key, err := legacyNativeAnyCheckpointString(work.frame.key, work.frame.keyCapacity)
	if err != nil {
		return err
	}
	saved, ready, err := work.graphValue(ctx, key)
	if err != nil || !ready {
		return err
	}
	work.frameRecord.Key = saved
	if err := work.frames.append(ctx, work.store, work.frameRecord); err != nil {
		return err
	}
	work.frame, work.frameIndex, work.framePart = work.frame.parent, work.frameIndex+1, 0
	work.frameRecord = legacyNativeAnyCheckpointFrame{}
	return nil
}

func (work *legacyNativeAnyCheckpoint) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.closed {
		return false, ErrIntegrity
	}
	work.started = true
	if err := work.checkSource(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	if work.sourceValidated < len(work.source.Parts) {
		limit := min(len(work.source.Parts), work.sourceValidated+legacyNativeCheckpointPageItems)
		for work.sourceValidated < limit {
			if err := legacyNativeAnyCheckpointSourcePart(work.source, work.sourceValidated); err != nil {
				return work.fail(err)
			}
			work.sourceValidated++
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	if err := work.advance(ctx); err != nil {
		return work.fail(err)
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkSource(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeAnyCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(legacyNativeAnyCheckpointSeenHeap(len(work.seen)))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.include(legacyNativeAnyCheckpointControlHeap(work.state))
	heap.include(legacyNativeAnyCheckpointDescriptorHeap(work.frameRecord.Owner))
	heap.include(legacyNativeAnyCheckpointDescriptorHeap(work.frameRecord.Key))
	heap.allocation(int64(len(work.root.Hash)), 1)
	work.parser.retained(&heap)
	work.guards.retained(&heap)
	for _, current := range []interface{ retainedBytes() (int64, error) }{&work.roots, &work.frames, work.graph, work.scalar, work.number, work.errorText} {
		if current != nil {
			bytes, err := current.retainedBytes()
			heap.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
		}
	}
	if work.identityHeap != nil && work.sharedField == nil {
		bytes, err := work.identityHeap()
		heap.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
	}
	if work.state.Error != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.state.Error)))
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeAnyCheckpoint) remaining() (int64, error) {
	bytes, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if bytes > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - bytes, nil
}

func (work *legacyNativeAnyCheckpoint) admit(additional int64) error {
	if err := work.refreshSharedBudget(); err != nil {
		return err
	}
	remaining, err := work.remaining()
	if err != nil {
		return err
	}
	if additional < 0 || additional > remaining {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeAnyCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeAnyCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
	if work == nil {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointRef{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		_, failure := work.fail(err)
		return legacyNativeCheckpointRef{}, failure
	}
	if work.closed {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if !work.ready {
		_, err := work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, err
	}
	if err := work.checkSource(); err != nil {
		_, failure := work.fail(err)
		return legacyNativeCheckpointRef{}, failure
	}
	if err := work.admit(0); err != nil {
		_, failure := work.fail(err)
		return legacyNativeCheckpointRef{}, failure
	}
	root := work.root
	work.close()
	return root, nil
}

func (work *legacyNativeAnyCheckpoint) close() {
	if work == nil || work.closed {
		return
	}
	for _, current := range []interface{ close() }{work.graph, work.scalar, work.number, work.errorText} {
		if current != nil {
			current.close()
		}
	}
	work.roots.close()
	work.frames.close()
	work.guards = legacyCanonicalPages[legacyNativeAnyCheckpointFrameGuard]{}
	if work.identityClose != nil {
		work.identityClose()
	}
	failure := work.failure
	*work = legacyNativeAnyCheckpoint{closed: true, failure: failure}
}

func legacyNativeAnyCheckpointCheckFrame(state legacyNativeAnyCheckpointControl, index int, frame *legacyNativeAnyFrame, owner, pending legacyNativeAnyValueOwner) error {
	if frame == nil || index < 0 || index >= state.Depth || frame.phase > legacyNativeAnyAfter ||
		(frame.object == nil) == (frame.array == nil) || frame.keyCapacity < len(frame.key) ||
		index > 0 && frame.phase != legacyNativeAnyValueNext ||
		state.Pending && legacyNativeAnyCheckpointOwnerKey(owner) == legacyNativeAnyCheckpointOwnerKey(pending) {
		return ErrIntegrity
	}
	if frame.array != nil {
		if frame.key != "" || frame.keyCapacity != 0 || frame.phase == legacyNativeAnyKey || frame.phase == legacyNativeAnyColon ||
			frame.phase == legacyNativeAnyFirst && len(frame.array.values()) != 0 || frame.phase == legacyNativeAnyAfter && len(frame.array.values()) == 0 ||
			frame.array.maximum > (state.End-state.From)/2 {
			return ErrIntegrity
		}
	} else if frame.phase != legacyNativeAnyColon && frame.phase != legacyNativeAnyValueNext && (frame.key != "" || frame.keyCapacity != 0) {
		return ErrIntegrity
	}
	if index == 0 {
		if state.KeyToken && (frame.object == nil || frame.phase != legacyNativeAnyKey) ||
			(state.Scanning || state.Applying || state.Pending) && !state.KeyToken && frame.phase != legacyNativeAnyValueNext ||
			state.OpeningRemaining > 0 && !(frame.array != nil && frame.phase == legacyNativeAnyFirst || frame.phase == legacyNativeAnyValueNext) ||
			state.ClosingRemaining > 0 && !(state.Pending && frame.phase == legacyNativeAnyValueNext ||
				!state.Pending && (frame.phase == legacyNativeAnyFirst || frame.phase == legacyNativeAnyAfter)) {
			return ErrIntegrity
		}
	}
	if index < state.ClosingRemaining && (state.ClosingByte == ']' && frame.array == nil || state.ClosingByte == '}' && frame.object == nil) {
		return ErrIntegrity
	}
	return nil
}

type legacyNativeAnyCheckpointRestore struct {
	sharedField     *legacyNativeObjectAnyCheckpointRestore
	sharedRegistry  *legacyNativeCheckpointGraphRestored
	state           legacyNativeAnyCheckpointControl
	root            legacyNativeCheckpointRef
	binding         ContentHash
	source          LegacyByteSource
	sourceValidated int
	store           legacyNativeCheckpointIO
	budget          int64
	parser          *legacyNativeAny
	scanner         legacyJSONValueScanner
	visible         legacyNativeAnyValue
	values          [legacyNativeAnyCheckpointRoots]legacyNativeAnyValue
	roots           *legacyNativeCheckpointListReader[legacyNativeAnyCheckpointRoot]
	frames          *legacyNativeCheckpointListReader[legacyNativeAnyCheckpointFrame]
	rootIndex       int
	frameIndex      int
	framePart       uint8
	frameRecord     legacyNativeAnyCheckpointFrame
	frameOwner      legacyNativeAnyValue
	frameKey        legacyNativeAnyValue
	lastFrame       *legacyNativeAnyFrame
	frameHeap       docRangeHeaderHeapEstimate
	seen            map[legacyNativeAnyValueOwner]struct{}
	graph           legacyNativeAnyCheckpointTask[legacyNativeAnyValue]
	scalar          legacyNativeAnyCheckpointTask[*legacyNativeScalar]
	number          legacyNativeAnyCheckpointNumberRecovery
	errorScalar     *legacyNativeScalar
	startGraph      func(context.Context, legacyNativeCheckpointValue, int64) (legacyNativeAnyCheckpointTask[legacyNativeAnyValue], error)
	registryHeap    func() (int64, error)
	registryClose   func()
	phase           uint8
	ready           bool
	closed          bool
	failure         error
}

func newLegacyNativeAnyCheckpointRestore(ctx context.Context, ref legacyNativeCheckpointRef, store legacyNativeCheckpointIO, currentLiveSource LegacyByteSource, binding ContentHash, heapBudget int64) (*legacyNativeAnyCheckpointRestore, error) {
	return newLegacyNativeAnyCheckpointRestoreInField(ctx, ref, store, currentLiveSource, binding, heapBudget, nil)
}

func newLegacyNativeAnyCheckpointRestoreInField(ctx context.Context, ref legacyNativeCheckpointRef, store legacyNativeCheckpointIO, currentLiveSource LegacyByteSource, binding ContentHash, heapBudget int64, field *legacyNativeObjectAnyCheckpointRestore) (*legacyNativeAnyCheckpointRestore, error) {
	if field != nil {
		parent, err := field.sharedRestoreRoot()
		if err != nil {
			return nil, err
		}
		if field.child != nil || field.state.Parser != ref || binding != parent.binding {
			return nil, ErrConflict
		}
		store = parent.store
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ref.valid() || store.Read == nil || ValidateContentHash(binding) != nil || heapBudget <= 0 {
		return nil, ErrIntegrity
	}
	if err := legacyNativeAnyCheckpointSourceGeometry(currentLiveSource, currentLiveSource.Bytes); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeAnyCheckpointRestore{})))
	planned.include(legacyNativeAnyCheckpointSeenHeap(0))
	planned.include(legacyNativeAnyCheckpointSourceHeap(currentLiveSource))
	planned.include(docRangeHeaderHeapEstimate{bytes: legacyNativeAnyCheckpointScratch})
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	work := &legacyNativeAnyCheckpointRestore{sharedField: field, root: ref, binding: binding, store: store, budget: heapBudget, source: currentLiveSource}
	if field != nil {
		field.child, work.sharedRegistry = work, field.registry
	}
	return work, nil
}

func (work *legacyNativeAnyCheckpointRestore) restoreValue(ctx context.Context, saved legacyNativeCheckpointValue) (legacyNativeAnyValue, bool, error) {
	if work.graph == nil {
		remaining, err := work.remaining()
		if err != nil {
			return legacyNativeAnyValue{}, false, err
		}
		shared, err := work.registryHeap()
		if err != nil {
			return legacyNativeAnyValue{}, false, err
		}
		work.graph, err = work.startGraph(ctx, saved, remaining+shared)
		if err != nil {
			return legacyNativeAnyValue{}, false, err
		}
	}
	ready, err := work.graph.step(ctx)
	if err != nil || !ready {
		return legacyNativeAnyValue{}, false, err
	}
	value, err := work.graph.take(ctx)
	work.graph.close()
	work.graph = nil
	if err == nil {
		err = value.validateOwnership()
	}
	return value, err == nil, err
}

func (work *legacyNativeAnyCheckpointRestore) advance(ctx context.Context) error {
	switch work.phase {
	case 0:
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, work.root, &work.state); err != nil {
			return err
		}
		if err := work.state.validate(work.binding, work.source.Bytes, true); err != nil {
			return err
		}
		registry := work.sharedRegistry
		if registry == nil {
			registry = newLegacyNativeCheckpointGraphRestored()
		}
		store := work.store
		work.startGraph = func(current context.Context, saved legacyNativeCheckpointValue, budget int64) (legacyNativeAnyCheckpointTask[legacyNativeAnyValue], error) {
			return newLegacyNativeCheckpointGraphRestoreInField(current, saved, store, registry, budget, work.sharedField, work.sharedGraphAny())
		}
		work.registryHeap = registry.retainedBytes
		if work.sharedField == nil {
			work.registryClose = registry.close
		}
		work.phase++
	case 1:
		var planned docRangeHeaderHeapEstimate
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeAny{})))
		planned.allocation(int64(work.state.RawCapacity), 1)
		if planned.err != nil {
			return planned.err
		}
		if err := work.admit(planned.bytes + legacyNativeAnyCheckpointScratch); err != nil {
			return err
		}
		state := work.state
		work.parser = &legacyNativeAny{from: state.From, end: state.End, position: state.Position, parentDepth: state.ParentDepth,
			depth: state.Depth, fieldLimit: state.FieldLimit, tokenStart: state.TokenStart, tokenEnd: state.TokenEnd,
			tokenFirst: state.TokenFirst, keyToken: state.KeyToken, scanning: state.Scanning, applying: state.Applying,
			pending: state.Pending, rootReserved: state.RootReserved, finished: state.Finished, errorContextSet: state.ErrorContextSet,
			openingRemaining: state.OpeningRemaining, closingRemaining: state.ClosingRemaining, closingByte: state.ClosingByte,
			raw: make([]byte, 0, state.RawCapacity)}
		work.seen = make(map[legacyNativeAnyValueOwner]struct{})
		work.phase++
	case 2:
		raw, err := legacyNativeCheckpointRead(ctx, work.store, work.state.Scanner)
		if err != nil {
			return err
		}
		work.scanner, err = restoreLegacyJSONValueScanner(ctx, raw)
		if err != nil {
			return err
		}
		if err := work.checkScanner(); err != nil {
			return err
		}
		work.phase++
	case 3:
		if !work.state.HasScalar {
			work.phase++
			return nil
		}
		if work.scalar == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			current, err := newLegacyNativeCheckpointScalarRestore(ctx, work.state.Scalar, work.store, work.source.Bytes, remaining)
			if err != nil {
				return err
			}
			work.scalar = current
		}
		ready, err := work.scalar.step(ctx)
		if err != nil || !ready {
			return err
		}
		work.parser.scalar, err = work.scalar.take(ctx)
		work.scalar.close()
		work.scalar = nil
		if err != nil {
			return err
		}
		work.phase++
	case 4:
		if work.state.Number == (legacyNativeCheckpointRef{}) {
			work.phase++
			return nil
		}
		if work.number == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			current, err := newLegacyNativeCheckpointNumberRestore(ctx, work.state.Number, work.store, work.source, remaining)
			if err != nil {
				return err
			}
			work.number = current
		}
		ready, err := work.number.step(ctx)
		if err != nil || !ready {
			return err
		}
		resumedSource := work.number.source()
		if err := legacyNativeAnyCheckpointSourceGeometry(resumedSource, work.source.Bytes); err != nil {
			return err
		}
		if resumedSource.authority != work.source.authority || resumedSource.partBytes() != work.source.partBytes() {
			return ErrIntegrity
		}
		work.parser.number, err = work.number.take(ctx)
		work.number.close()
		work.number = nil
		if err != nil {
			return err
		}
		work.source = resumedSource
		number := work.parser.number
		if number == nil || number.parser == nil || number.parser.from != work.state.TokenStart || number.parser.end != work.state.TokenEnd {
			return ErrIntegrity
		}
		work.phase++
	case 5:
		if work.state.Error == nil {
			work.phase++
			return nil
		}
		if work.scalar == nil {
			remaining, err := work.remaining()
			if err != nil {
				return err
			}
			current, err := newLegacyNativeCheckpointScalarRestore(ctx, work.state.Error.Value, work.store, work.source.Bytes, remaining)
			if err != nil {
				return err
			}
			work.scalar = current
		}
		ready, err := work.scalar.step(ctx)
		if err != nil || !ready {
			return err
		}
		work.errorScalar, err = work.scalar.take(ctx)
		work.scalar.close()
		work.scalar = nil
		if err != nil {
			return err
		}
		if work.errorScalar == nil || work.errorScalar.text == nil || len(work.errorScalar.text.String()) < len("number ") ||
			work.errorScalar.text.String()[:len("number ")] != "number " {
			return ErrIntegrity
		}
		semantic := work.state.Error
		work.parser.semanticError = &json.UnmarshalTypeError{Value: work.errorScalar.text.String(), Type: reflect.TypeFor[float64](),
			Offset: semantic.Offset, Struct: semantic.Struct, Field: semantic.Field}
		work.phase++
	case 6:
		var err error
		work.roots, err = newLegacyNativeCheckpointListReader[legacyNativeAnyCheckpointRoot](work.state.Roots, work.store)
		if err != nil {
			return err
		}
		work.phase++
	case 7:
		return work.restoreRoot(ctx)
	case 8:
		if err := work.bindRoots(); err != nil {
			return err
		}
		var err error
		work.frames, err = newLegacyNativeCheckpointListReader[legacyNativeAnyCheckpointFrame](work.state.Frames, work.store)
		if err != nil {
			return err
		}
		work.phase++
	case 9:
		return work.restoreFrame(ctx)
	case 10:
		if err := work.validateComplete(); err != nil {
			return err
		}
		work.ready = true
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeAnyCheckpointRestore) restoreRoot(ctx context.Context) error {
	if work.framePart == 0 {
		record, found, err := work.roots.next(ctx)
		if err != nil {
			return err
		}
		if !found {
			if work.rootIndex != legacyNativeAnyCheckpointRoots {
				return ErrIntegrity
			}
			work.roots.close()
			work.roots = nil
			work.phase++
			return nil
		}
		if work.rootIndex >= legacyNativeAnyCheckpointRoots || int(record.Slot) != work.rootIndex {
			return ErrIntegrity
		}
		work.frameRecord.Owner, work.framePart = record.Value, 1
		return nil
	}
	value, ready, err := work.restoreValue(ctx, work.frameRecord.Owner)
	if err != nil || !ready {
		return err
	}
	work.values[work.rootIndex] = value
	work.rootIndex++
	work.frameRecord, work.framePart = legacyNativeAnyCheckpointFrame{}, 0
	return nil
}

func (work *legacyNativeAnyCheckpointRestore) bindRoots() error {
	parser := work.parser
	rootMap, rootArray, node, result, visible := work.values[0], work.values[1], work.values[2], work.values[3], work.values[4]
	if rootMap.value != nil && rootMap.owner.object == nil || rootArray.value != nil && rootArray.owner.array == nil ||
		rootMap.owner.array != nil || rootArray.owner.object != nil || rootMap.owner.object != nil && rootArray.owner.array != nil ||
		!parser.pending && node.value != nil || !parser.finished && result.value != nil ||
		visible.value != nil && visible.owner == (legacyNativeAnyValueOwner{}) {
		return ErrIntegrity
	}
	parser.rootMap, parser.rootArray, parser.node, parser.result = rootMap.owner.object, rootArray.owner.array, node, result
	parser.visible, work.visible = visible.owner, visible
	return legacyNativeAnyCheckpointCheckRoots(parser)
}

func (work *legacyNativeAnyCheckpointRestore) restoreFrame(ctx context.Context) error {
	if work.framePart == 0 {
		record, found, err := work.frames.next(ctx)
		if err != nil {
			return err
		}
		if !found {
			if work.frameIndex != work.state.Depth {
				return ErrIntegrity
			}
			work.frames.close()
			work.frames = nil
			work.phase++
			return nil
		}
		if work.frameIndex >= work.state.Depth || record.Index != uint32(work.frameIndex) || record.Phase > legacyNativeAnyAfter {
			return ErrIntegrity
		}
		work.frameRecord, work.framePart = record, 1
		return nil
	}
	if work.framePart == 1 {
		value, ready, err := work.restoreValue(ctx, work.frameRecord.Owner)
		if err != nil || !ready {
			return err
		}
		if value.owner == (legacyNativeAnyValueOwner{}) {
			return ErrIntegrity
		}
		work.frameOwner, work.framePart = value, 2
		return nil
	}
	if work.framePart == 2 {
		value, ready, err := work.restoreValue(ctx, work.frameRecord.Key)
		if err != nil || !ready {
			return err
		}
		if _, ok := value.value.(string); !ok || value.owner != (legacyNativeAnyValueOwner{}) {
			return ErrIntegrity
		}
		work.frameKey, work.framePart = value, 3
		return nil
	}
	owner := legacyNativeAnyCheckpointOwnerKey(work.frameOwner.owner)
	if _, found := work.seen[owner]; found {
		return ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeAnyFrame{})))
	planned.allocation(1, 128)
	if err := work.admit(planned.bytes + legacyNativeAnyCheckpointScratch); err != nil {
		return err
	}
	frame := &legacyNativeAnyFrame{object: owner.object, array: owner.array, key: work.frameKey.value.(string),
		keyCapacity: work.frameKey.capacity, phase: work.frameRecord.Phase}
	if err := legacyNativeAnyCheckpointCheckFrame(work.state, work.frameIndex, frame, work.frameOwner.owner, work.parser.node.owner); err != nil {
		return err
	}
	rootOwner := legacyNativeAnyValueOwner{object: work.parser.rootMap, array: work.parser.rootArray}
	if (work.frameIndex == work.state.Depth-1) != (owner == rootOwner) {
		return ErrIntegrity
	}
	estimate := frame.estimate()
	if estimate.err != nil {
		return estimate.err
	}
	frame.retained = estimate.bytes
	work.parser.live.include(estimate)
	work.frameHeap.include(frame.headerHeap())
	if work.parser.live.err != nil || work.frameHeap.err != nil {
		return ErrIntegrity
	}
	if work.lastFrame == nil {
		work.parser.frame = frame
	} else {
		work.lastFrame.parent = frame
	}
	work.lastFrame = frame
	work.seen[owner] = struct{}{}
	work.frameIndex++
	work.framePart = 0
	work.frameRecord, work.frameOwner, work.frameKey = legacyNativeAnyCheckpointFrame{}, legacyNativeAnyValue{}, legacyNativeAnyValue{}
	return nil
}

func legacyNativeAnyCheckpointCheckScanner(state legacyNativeAnyCheckpointControl, scanner *legacyJSONValueScanner) error {
	if scanner.invalid {
		return ErrIntegrity
	}
	if !state.Scanning {
		if state.Applying && !scanner.complete {
			return ErrIntegrity
		}
		return nil
	}
	if scanner.complete || scanner.depth != 0 || scanner.parents != state.ParentDepth+state.Depth || scanner.key {
		return ErrIntegrity
	}
	if state.Position == state.TokenStart {
		if scanner.mode != legacyJSONValueIdle {
			return ErrIntegrity
		}
		return nil
	}
	switch state.TokenFirst {
	case '"':
		if scanner.mode != legacyJSONValueString && scanner.mode != legacyJSONValueEscape && scanner.mode != legacyJSONValueUnicode {
			return ErrIntegrity
		}
	case 't', 'f', 'n':
		expected := "true"
		if state.TokenFirst == 'f' {
			expected = "false"
		} else if state.TokenFirst == 'n' {
			expected = "null"
		}
		if scanner.mode != legacyJSONValueLiteral || scanner.literal != expected || int(scanner.literalNext) != state.Position-state.TokenStart {
			return ErrIntegrity
		}
	default:
		if scanner.mode < legacyJSONValueMinus || scanner.mode > legacyJSONValueExponentDigits {
			return ErrIntegrity
		}
	}
	return nil
}

func (work *legacyNativeAnyCheckpointRestore) checkScanner() error {
	return legacyNativeAnyCheckpointCheckScanner(work.state, &work.scanner)
}

func (work *legacyNativeAnyCheckpointRestore) validateComplete() error {
	if work.parser == nil || work.rootIndex != legacyNativeAnyCheckpointRoots || work.frameIndex != work.state.Depth ||
		work.graph != nil || work.scalar != nil || work.number != nil || work.framePart != 0 ||
		(work.parser.frame == nil) != (work.state.Depth == 0) || len(work.seen) != work.state.Depth {
		return ErrIntegrity
	}
	if err := work.bindRoots(); err != nil {
		return err
	}
	if err := work.checkScanner(); err != nil {
		return err
	}
	if work.parser.pending && work.parser.node.owner == (legacyNativeAnyValueOwner{}) {
		if !work.scanner.complete || work.state.TokenEnd <= work.state.TokenStart || work.state.TokenEnd != work.state.Position {
			return ErrIntegrity
		}
	}
	if work.parser.semanticError != nil && !work.state.Finished && work.state.ErrorContextSet {
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeAnyCheckpointRestore) step(ctx context.Context) (bool, error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if err := work.refreshSharedBudget(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	if work.phase > 0 && work.sourceValidated < len(work.source.Parts) {
		limit := min(len(work.source.Parts), work.sourceValidated+legacyNativeCheckpointPageItems)
		for work.sourceValidated < limit {
			if err := legacyNativeAnyCheckpointSourcePart(work.source, work.sourceValidated); err != nil {
				return work.fail(err)
			}
			work.sourceValidated++
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	if err := work.advance(ctx); err != nil {
		return work.fail(err)
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeAnyCheckpointRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(legacyNativeAnyCheckpointSeenHeap(len(work.seen)))
	heap.include(legacyNativeAnyCheckpointSourceHeap(work.source))
	heap.include(legacyNativeAnyCheckpointControlHeap(work.state))
	heap.include(legacyNativeAnyCheckpointDescriptorHeap(work.frameRecord.Owner))
	heap.include(legacyNativeAnyCheckpointDescriptorHeap(work.frameRecord.Key))
	heap.allocation(int64(len(work.root.Hash)+len(work.binding)), 1)
	heap.include(work.frameHeap)
	if work.parser != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.parser)))
		heap.allocation(int64(cap(work.parser.raw)), 1)
		if work.parser.scalar != nil {
			work.parser.scalar.retained(&heap)
		}
		work.parser.number.retained(&heap)
		if work.parser.semanticError != nil {
			heap.allocation(1, int64(unsafe.Sizeof(*work.parser.semanticError)))
		}
	}
	for _, value := range work.values {
		if value.owner == (legacyNativeAnyValueOwner{}) {
			heap.include(value.heap)
		}
	}
	if work.frameKey.owner == (legacyNativeAnyValueOwner{}) {
		heap.include(work.frameKey.heap)
	}
	if work.errorScalar != nil {
		work.errorScalar.retained(&heap)
	}
	for _, current := range []interface{ retainedBytes() (int64, error) }{work.roots, work.frames, work.graph, work.scalar, work.number} {
		if current != nil {
			bytes, err := current.retainedBytes()
			heap.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
		}
	}
	if work.registryHeap != nil && work.sharedField == nil {
		bytes, err := work.registryHeap()
		heap.include(docRangeHeaderHeapEstimate{bytes: bytes, err: err})
	}
	if work.state.Error != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.state.Error)))
		heap.allocation(int64(len(work.state.Error.Struct)+len(work.state.Error.Field)), 1)
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeAnyCheckpointRestore) remaining() (int64, error) {
	bytes, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if bytes > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - bytes, nil
}

func (work *legacyNativeAnyCheckpointRestore) admit(additional int64) error {
	if err := work.refreshSharedBudget(); err != nil {
		return err
	}
	remaining, err := work.remaining()
	if err != nil {
		return err
	}
	if additional < 0 || additional > remaining {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeAnyCheckpointRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeAnyCheckpointRestore) take(ctx context.Context) (legacyNativeAnyRestored, error) {
	if work == nil {
		return legacyNativeAnyRestored{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeAnyRestored{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		_, failure := work.fail(err)
		return legacyNativeAnyRestored{}, failure
	}
	if work.closed {
		return legacyNativeAnyRestored{}, ErrIntegrity
	}
	if !work.ready {
		_, err := work.fail(ErrIntegrity)
		return legacyNativeAnyRestored{}, err
	}
	if err := work.validateComplete(); err != nil {
		_, failure := work.fail(err)
		return legacyNativeAnyRestored{}, failure
	}
	if err := work.admit(0); err != nil {
		_, failure := work.fail(err)
		return legacyNativeAnyRestored{}, failure
	}
	shared, err := work.sharedParserBytes()
	if err != nil {
		_, failure := work.fail(err)
		return legacyNativeAnyRestored{}, failure
	}
	result := legacyNativeAnyRestored{Parser: work.parser, Scanner: work.scanner, Visible: work.visible, Source: work.source, GraphBytes: shared}
	work.close()
	return result, nil
}

func (work *legacyNativeAnyCheckpointRestore) close() {
	if work == nil || work.closed {
		return
	}
	for _, current := range []interface{ close() }{work.graph, work.scalar, work.number} {
		if current != nil {
			current.close()
		}
	}
	if work.roots != nil {
		work.roots.close()
	}
	if work.frames != nil {
		work.frames.close()
	}
	if work.registryClose != nil {
		work.registryClose()
	}
	failure := work.failure
	*work = legacyNativeAnyCheckpointRestore{closed: true, failure: failure}
}
