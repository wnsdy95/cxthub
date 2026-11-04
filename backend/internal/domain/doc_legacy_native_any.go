package domain

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"unsafe"
)

type legacyNativeAnyValue struct {
	value    any
	heap     docRangeHeaderHeapEstimate
	excess   int64
	capacity int
	owner    legacyNativeAnyValueOwner
}

const legacyNativeAnyValueActions = ConversationManifestChunkBytes / int(unsafe.Sizeof(any(nil)))

const (
	legacyNativeAnyFirst uint8 = iota
	legacyNativeAnyKey
	legacyNativeAnyColon
	legacyNativeAnyValueNext
	legacyNativeAnyAfter
)

type legacyNativeAnyFrame struct {
	parent      *legacyNativeAnyFrame
	object      *legacyNativeAnyMap
	array       *legacyNativeAnyArray
	key         string
	keyCapacity int
	retained    int64
	phase       uint8
}

func (current *legacyNativeAnyFrame) headerHeap() docRangeHeaderHeapEstimate {
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(*current)))
	estimate.allocation(int64(current.keyCapacity), 1)
	return estimate
}

func (current *legacyNativeAnyFrame) estimate() docRangeHeaderHeapEstimate {
	estimate := current.headerHeap()
	if current.object != nil {
		estimate.include(current.object.heap())
		current.object.retained(&estimate)
	}
	if current.array != nil {
		estimate.include(current.array.heap())
		current.array.retained(&estimate)
	}
	return estimate
}

type legacyNativeAny struct {
	frame            *legacyNativeAnyFrame
	rootMap          *legacyNativeAnyMap
	rootArray        *legacyNativeAnyArray
	scalar           *legacyNativeScalar
	number           *legacyNativeNumberValue
	node             legacyNativeAnyValue
	result           legacyNativeAnyValue
	visible          legacyNativeAnyValueOwner
	live             docRangeHeaderHeapEstimate
	raw              []byte
	semanticError    *json.UnmarshalTypeError
	position         int
	from             int
	end              int
	parentDepth      int
	depth            int
	fieldLimit       int
	tokenStart       int
	tokenEnd         int
	tokenFirst       byte
	keyToken         bool
	scanning         bool
	applying         bool
	pending          bool
	rootReserved     bool
	finished         bool
	errorContextSet  bool
	openingRemaining int
	closingRemaining int
	closingByte      byte
}

func newLegacyNativeAny(from, to, parentDepth, fieldLimit int) (*legacyNativeAny, error) {
	parser := &legacyNativeAny{finished: true}
	if err := parser.start(from, to, parentDepth, fieldLimit); err != nil {
		return nil, err
	}
	return parser, nil
}

func (parser *legacyNativeAny) start(from, to, parentDepth, fieldLimit int) error {
	if !parser.finished || from < 0 || to <= from || parentDepth < 0 || parentDepth >= 10000 || fieldLimit <= 0 {
		return ErrIntegrity
	}
	parser.frame, parser.scalar = nil, nil
	parser.number = nil
	parser.node, parser.result = legacyNativeAnyValue{}, legacyNativeAnyValue{}
	parser.live = docRangeHeaderHeapEstimate{}
	parser.raw = parser.raw[:0]
	parser.semanticError = nil
	parser.position, parser.end, parser.parentDepth, parser.fieldLimit = from, to, parentDepth, fieldLimit
	parser.from, parser.errorContextSet = from, false
	parser.depth, parser.tokenStart, parser.tokenEnd, parser.tokenFirst = 0, 0, 0, 0
	parser.keyToken, parser.scanning, parser.applying, parser.pending = false, false, false, false
	parser.rootReserved, parser.finished = parser.rootMap != nil, false
	parser.openingRemaining, parser.closingRemaining, parser.closingByte = 0, 0, 0
	parser.rootArray = nil
	return nil
}

func (parser *legacyNativeAny) syncFrame(current *legacyNativeAnyFrame) error {
	return parser.syncFrameEstimate(current, current.estimate())
}

func (parser *legacyNativeAny) syncFrameEstimate(current *legacyNativeAnyFrame, next docRangeHeaderHeapEstimate) error {
	live, err := parser.frameTotal(current, next)
	if err != nil {
		return err
	}
	parser.live = live
	current.retained = next.bytes
	return nil
}

func (parser *legacyNativeAny) frameTotal(current *legacyNativeAnyFrame, next docRangeHeaderHeapEstimate) (docRangeHeaderHeapEstimate, error) {
	if next.err != nil || parser.live.err != nil || parser.live.bytes < current.retained {
		return docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	live := parser.live
	live.bytes -= current.retained
	live.include(next)
	return live, live.err
}

func (parser *legacyNativeAny) open(current byte, input *legacyNormalizationInput) (bool, error) {
	if parser.parentDepth+parser.depth+1 > 10000 {
		return false, ErrIntegrity
	}
	cost := int(unsafe.Sizeof(legacyNativeAnyFrame{}))
	if current == '[' {
		cost += int(unsafe.Sizeof(legacyNativeAnyArray{}))
	} else if parser.frame != nil || !parser.rootReserved {
		cost += int(unsafe.Sizeof(legacyNativeAnyMap{})) + 128
	}
	if cost > ConversationManifestChunkBytes-input.workBytes {
		return false, nil
	}
	next := &legacyNativeAnyFrame{parent: parser.frame, phase: legacyNativeAnyFirst}
	reuse := current == '{' && parser.frame == nil && parser.rootReserved
	if current == '{' {
		if reuse {
			next.object = parser.rootMap
		} else {
			next.object = newLegacyNativeAnyMap()
		}
	} else {
		var err error
		next.array, err = newLegacyNativeAnyArray((parser.end - parser.position) / 2)
		if err != nil {
			return false, err
		}
	}
	estimate := next.estimate()
	live, err := parser.frameTotal(next, estimate)
	if err != nil {
		return false, err
	}
	input.workBytes += cost
	if reuse {
		parser.rootReserved = false
	}
	if parser.frame == nil {
		parser.rootMap, parser.rootArray = next.object, next.array
	}
	parser.frame = next
	parser.depth++
	parser.position++
	parser.live, next.retained = live, estimate.bytes
	return true, nil
}

func (parser *legacyNativeAny) close() error {
	current := parser.frame
	if current == nil || parser.live.err != nil || parser.live.bytes < current.retained {
		return ErrIntegrity
	}
	var next legacyNativeAnyValue
	if current.object != nil {
		next.value, next.heap, next.excess = current.object.values(), current.object.heap(), current.object.backingExcess()
	} else {
		next.value, next.heap, next.excess = current.array.values(), current.array.heap(), current.array.backingExcess()
	}
	if next.heap.err != nil {
		return next.heap.err
	}
	var retained docRangeHeaderHeapEstimate
	if current.object != nil {
		current.object.retained(&retained)
		next.owner = legacyNativeAnyValueOwner{object: current.object, revision: current.object.revision, bytes: retained.bytes}
	} else {
		current.array.retained(&retained)
		next.owner = legacyNativeAnyValueOwner{array: current.array, revision: current.array.revision, bytes: retained.bytes}
	}
	if retained.err != nil {
		return retained.err
	}
	parser.live.bytes -= current.retained
	parser.frame, parser.depth = current.parent, parser.depth-1
	parser.position++
	if parser.frame == nil {
		parser.result, parser.finished = next, true
		parser.rootMap, parser.rootArray = current.object, current.array
		parser.rootReserved = true
	} else {
		parser.node, parser.pending = next, true
	}
	return nil
}

func (parser *legacyNativeAny) beginToken(current byte, key bool, frame *legacyJSONValueScanner) error {
	if err := frame.reset(parser.parentDepth + parser.depth); err != nil {
		return err
	}
	parser.tokenStart, parser.tokenFirst, parser.keyToken = parser.position, current, key
	parser.scanning = true
	return nil
}

func (parser *legacyNativeAny) structuralRun(ctx context.Context, input *legacyNormalizationInput, first byte, maximum int) (int, error) {
	if maximum <= 0 {
		return 0, ErrIntegrity
	}
	count := 1
	for count < maximum && parser.position+count < parser.end && ConversationManifestChunkBytes-input.workBytes >= 2 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		input.workBytes++
		current, err := input.current(parser.position + count)
		if errors.Is(err, errLegacyNormalizationYield) {
			break
		}
		if err != nil {
			return 0, err
		}
		if current != first {
			break
		}
		count++
	}
	return count, ctx.Err()
}

func (parser *legacyNativeAny) beginArrayRun(ctx context.Context, input *legacyNormalizationInput) error {
	count, err := parser.structuralRun(ctx, input, '[', 10000-parser.parentDepth-parser.depth)
	if err != nil {
		return err
	}
	parser.openingRemaining = count
	return nil
}

func (parser *legacyNativeAny) beginClosingRun(ctx context.Context, input *legacyNormalizationInput, first byte) error {
	count, err := parser.structuralRun(ctx, input, first, parser.depth)
	if err != nil {
		return err
	}
	if err := parser.close(); err != nil {
		return err
	}
	parser.closingRemaining, parser.closingByte = count-1, 0
	if parser.closingRemaining > 0 {
		parser.closingByte = first
	}
	return nil
}

func (parser *legacyNativeAny) commit(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	current := parser.frame
	if current == nil || !parser.pending {
		return false, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var next docRangeHeaderHeapEstimate
	guard := func(owned docRangeHeaderHeapEstimate) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var frame docRangeHeaderHeapEstimate
		frame.allocation(1, int64(unsafe.Sizeof(*current)))
		frame.include(owned)
		_, err := parser.frameTotal(current, frame)
		return err
	}
	if parser.keyToken {
		text, ok := parser.node.value.(string)
		if !ok || current.object == nil {
			return false, ErrIntegrity
		}
		next = current.estimate()
		var previousKey docRangeHeaderHeapEstimate
		previousKey.allocation(int64(current.keyCapacity), 1)
		if next.err != nil || previousKey.err != nil || next.bytes < previousKey.bytes {
			return false, ErrIntegrity
		}
		next.bytes -= previousKey.bytes
		next.allocation(int64(parser.node.capacity), 1)
		if _, err := parser.frameTotal(current, next); err != nil {
			return false, err
		}
		current.key, current.keyCapacity = text, parser.node.capacity
		current.phase = legacyNativeAnyColon
	} else if current.object != nil {
		cost, err := current.object.putBytes(current.key)
		if err != nil {
			return false, err
		}
		if cost > ConversationManifestChunkBytes-input.workBytes {
			return false, nil
		}
		owned, err := current.object.putValueGuarded(current.key, current.keyCapacity, parser.node, guard)
		if err != nil {
			return false, err
		}
		input.workBytes += cost
		current.key, current.keyCapacity = "", 0
		current.phase = legacyNativeAnyAfter
		next = current.headerHeap()
		next.include(owned)
	} else {
		ready, err := current.array.appendValueGuarded(ctx, input, parser.node, guard)
		if syncErr := parser.syncFrame(current); syncErr != nil {
			return false, syncErr
		}
		if err != nil || !ready {
			return false, err
		}
		current.phase = legacyNativeAnyAfter
		next = current.estimate()
	}
	if err := parser.syncFrameEstimate(current, next); err != nil {
		return false, err
	}
	parser.node, parser.pending, parser.keyToken = legacyNativeAnyValue{}, false, false
	input.values++
	return true, nil
}

func (parser *legacyNativeAny) decodeToken(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if parser.tokenFirst == '"' {
		if parser.scalar == nil {
			parser.scalar = &legacyNativeScalar{position: parser.tokenStart + 1, end: parser.tokenEnd - 1, maximum: 3 * (parser.tokenEnd - parser.tokenStart - 2)}
		}
		ready, err := parser.scalar.step(ctx, input)
		if err != nil || !ready {
			return false, err
		}
		text, capacity := parser.scalar.text.String(), parser.scalar.text.Cap()
		var heap docRangeHeaderHeapEstimate
		heap.allocation(1, int64(unsafe.Sizeof(text)))
		heap.allocation(int64(capacity), 1)
		parser.node = legacyNativeAnyValue{value: text, heap: heap, excess: 2 * int64(capacity-len(text)), capacity: capacity}
		parser.scalar = nil
	} else {
		length := parser.tokenEnd - parser.tokenStart
		var value any
		var err error
		if length > LegacyCanonicalEncodingBytes {
			if parser.tokenFirst != '-' && (parser.tokenFirst < '0' || parser.tokenFirst > '9') {
				return false, ErrIntegrity
			}
			if parser.number == nil {
				parser.number, err = newLegacyNativeNumberValue(parser.tokenStart, parser.tokenEnd, input.numberDiagnostics)
				if err != nil {
					return false, err
				}
			}
			var complete bool
			complete, err = parser.number.step(ctx, input)
			if !complete {
				return false, err
			}
			if err == nil {
				value = parser.number.value
			}
			parser.number = nil
		} else {
			if !input.canDecode(parser.tokenStart, parser.tokenEnd) {
				return false, nil
			}
			input.semanticBytes += length
			reader := &legacyNormalizationValueReader{input: input, position: parser.tokenStart, end: parser.tokenEnd}
			if cap(parser.raw) < length {
				parser.raw = make([]byte, length)
			} else {
				parser.raw = parser.raw[:length]
			}
			if _, err = io.ReadFull(reader, parser.raw); err == nil {
				err = json.Unmarshal(parser.raw, &value)
			}
		}
		if err != nil {
			var semantic *json.UnmarshalTypeError
			if input.failure != nil || ctx.Err() != nil || !errors.As(err, &semantic) {
				return false, err
			}
			if parser.semanticError == nil {
				parser.semanticError = semantic
				parser.semanticError.Offset += int64(parser.tokenStart - parser.from)
			}
			value = nil
		}
		heap, err := legacyNativeValueHeap(ctx, reflect.ValueOf(value))
		if err != nil {
			return false, err
		}
		parser.node = legacyNativeAnyValue{value: value, heap: heap}
	}
	parser.applying, parser.pending = false, true
	return true, nil
}

func (parser *legacyNativeAny) step(ctx context.Context, input *legacyNormalizationInput, frame *legacyJSONValueScanner) (bool, error) {
	checked := input.workBytes
	for input.workBytes < ConversationManifestChunkBytes && input.fields < parser.fieldLimit && input.values < legacyNativeAnyValueActions {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if parser.finished {
			if parser.openingRemaining != 0 || parser.closingRemaining != 0 || parser.closingByte != 0 {
				return false, ErrIntegrity
			}
			if parser.semanticError != nil {
				return true, parser.semanticError
			}
			return true, nil
		}
		if parser.pending {
			ready, err := parser.commit(ctx, input)
			if err != nil || !ready {
				return false, err
			}
			continue
		}
		if parser.applying {
			ready, err := parser.decodeToken(ctx, input)
			if err != nil || !ready {
				return false, err
			}
			continue
		}
		if parser.openingRemaining > 0 {
			if parser.openingRemaining > 10000-parser.parentDepth-parser.depth || parser.frame != nil &&
				!(parser.frame.array != nil && parser.frame.phase == legacyNativeAnyFirst || parser.frame.phase == legacyNativeAnyValueNext) {
				return false, ErrIntegrity
			}
			if parser.frame != nil && parser.frame.array != nil && parser.frame.phase == legacyNativeAnyFirst {
				parser.frame.phase = legacyNativeAnyValueNext
			}
			if ready, err := parser.open('[', input); err != nil || !ready {
				return false, err
			}
			parser.openingRemaining--
			continue
		}
		if parser.closingRemaining > 0 {
			active := parser.frame
			if parser.closingRemaining > parser.depth || parser.closingByte != ']' && parser.closingByte != '}' ||
				active == nil || active.phase != legacyNativeAnyFirst && active.phase != legacyNativeAnyAfter ||
				parser.closingByte == ']' && active.array == nil || parser.closingByte == '}' && active.object == nil {
				return false, ErrIntegrity
			}
			if err := parser.close(); err != nil {
				return false, err
			}
			parser.closingRemaining--
			if parser.closingRemaining == 0 {
				parser.closingByte = 0
			}
			continue
		}
		if parser.position >= parser.end {
			return false, ErrIntegrity
		}
		current, err := input.current(parser.position)
		if errors.Is(err, errLegacyNormalizationYield) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if parser.scanning {
			consumed, complete, err := frame.add(current)
			if err != nil || !consumed && !complete {
				return false, ErrIntegrity
			}
			if consumed {
				parser.position++
			}
			if complete {
				parser.tokenEnd = parser.position
				parser.scanning, parser.applying = false, true
			}
			continue
		}
		if legacyNormalizationSpace(current) {
			parser.position++
			continue
		}
		if parser.frame == nil {
			if current != '{' && current != '[' {
				return false, ErrIntegrity
			}
			if current == '[' {
				if err := parser.beginArrayRun(ctx, input); err != nil {
					return false, err
				}
				continue
			}
			if ready, err := parser.open(current, input); err != nil || !ready {
				return false, err
			}
			continue
		}
		active := parser.frame
		switch active.phase {
		case legacyNativeAnyFirst:
			if active.object != nil && current == '}' || active.array != nil && current == ']' {
				if err := parser.beginClosingRun(ctx, input, current); err != nil {
					return false, err
				}
				continue
			}
			if active.object != nil {
				active.phase = legacyNativeAnyKey
			} else {
				active.phase = legacyNativeAnyValueNext
			}
			input.workBytes--
		case legacyNativeAnyKey:
			if current != '"' {
				return false, ErrIntegrity
			}
			if err := parser.beginToken(current, true, frame); err != nil {
				return false, err
			}
			input.workBytes--
		case legacyNativeAnyColon:
			if current != ':' {
				return false, ErrIntegrity
			}
			parser.position++
			active.phase = legacyNativeAnyValueNext
		case legacyNativeAnyValueNext:
			if current == '[' {
				if err := parser.beginArrayRun(ctx, input); err != nil {
					return false, err
				}
				continue
			}
			if current == '{' || current == '[' {
				if ready, err := parser.open(current, input); err != nil || !ready {
					return false, err
				}
			} else {
				if err := parser.beginToken(current, false, frame); err != nil {
					return false, err
				}
				input.workBytes--
			}
		case legacyNativeAnyAfter:
			if active.object != nil && current == '}' || active.array != nil && current == ']' {
				if err := parser.beginClosingRun(ctx, input, current); err != nil {
					return false, err
				}
			} else if current == ',' {
				parser.position++
				if active.object != nil {
					active.phase = legacyNativeAnyKey
				} else {
					active.phase = legacyNativeAnyValueNext
				}
			} else {
				return false, ErrIntegrity
			}
		default:
			return false, ErrIntegrity
		}
		if input.workBytes-checked >= canonicalCheckBytes {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			checked = input.workBytes
		}
	}
	return false, ctx.Err()
}

func (parser *legacyNativeAny) retained(estimate *docRangeHeaderHeapEstimate) {
	if parser == nil {
		return
	}
	estimate.allocation(1, int64(unsafe.Sizeof(*parser)))
	estimate.allocation(int64(cap(parser.raw)), 1)
	estimate.include(parser.live)
	estimate.include(docRangeHeaderHeapEstimate{bytes: parser.visible.bytes})
	if parser.rootReserved {
		if parser.rootMap != nil {
			estimate.include(parser.rootMap.heap())
			parser.rootMap.retained(estimate)
		}
		if parser.rootArray != nil {
			estimate.include(parser.rootArray.heap())
			parser.rootArray.retained(estimate)
		}
	}
	if parser.pending {
		estimate.include(parser.node.heap)
		estimate.include(docRangeHeaderHeapEstimate{bytes: parser.node.owner.bytes})
	}
	if parser.scalar != nil {
		parser.scalar.retained(estimate)
	}
	parser.number.retained(estimate)
	if parser.semanticError != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*parser.semanticError)))
		estimate.allocation(int64(len(parser.semanticError.Value)+len(parser.semanticError.Struct)+len(parser.semanticError.Field)), 1)
	}
}
