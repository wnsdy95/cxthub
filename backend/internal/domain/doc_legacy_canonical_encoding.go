package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"
	"unsafe"
)

const LegacyCanonicalEncodingBytes = 64 << 10

const legacyCanonicalEncodingActions = 1024

type LegacyCanonicalEncoding struct {
	root             any
	rootHeap         docRangeHeaderHeapEstimate
	mapKeys          legacyCanonicalPages[legacyCanonicalEncodingKeys]
	pendingKeysHeap  docRangeHeaderHeapEstimate
	frames           legacyCanonicalPages[legacyCanonicalEncodingFrame]
	order            legacyCanonicalKeyOrder
	keyWork          uint64
	ordering         bool
	initialized      bool
	complete         bool
	err              error
	numbers          legacyCanonicalNumberValidation
	eagerNumbers     bool
	nativeOwner      *legacyNativeCanonicalOwner
	preparation      *legacyCanonicalPreparation
	inspectWork      uint64
	lastStorageBytes int
}

type legacyCanonicalEncodingKeys struct {
	source  any
	fields  legacyCanonicalPages[legacyCanonicalKeyField]
	heap    int64
	endKeys int
	sorted  bool
}

type legacyCanonicalEncodingFrame struct {
	value        any
	block        ContentBlock
	fields       legacyCanonicalPages[legacyCanonicalKeyField]
	keysPosition int
	keysEnd      int
	text         string
	position     int
	phase        byte
	kind         byte
	ownedLiteral bool
	ordered      bool
}

func newLegacyCanonicalEncoding(ctx context.Context, value any) (*LegacyCanonicalEncoding, error) {
	return newLegacyCanonicalEncodingWithNumbers(ctx, value, true)
}

func newLegacyCanonicalEncodingPending(ctx context.Context, value any) (*LegacyCanonicalEncoding, error) {
	return newLegacyCanonicalEncodingWithNumbers(ctx, value, false)
}

func newLegacyCanonicalEncodingWithNumbers(ctx context.Context, value any, eager bool) (*LegacyCanonicalEncoding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	encoding := &LegacyCanonicalEncoding{root: value, initialized: true, eagerNumbers: eager}
	heap, err := encoding.inspect(ctx, value, 2, true)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, err
	}
	encoding.rootHeap = heap
	if err := encoding.frames.append(legacyCanonicalEncodingFrame{value: value}); err != nil {
		encoding.release()
		return nil, err
	}
	encoding.inspectWork++
	if _, err := encoding.RetainedBytes(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return encoding, nil
}

func (encoding *LegacyCanonicalEncoding) inspect(ctx context.Context, input any, depth int, prepare bool) (docRangeHeaderHeapEstimate, error) {
	preparation := newLegacyCanonicalPreparation(input, depth, prepare)
	for {
		complete, err := preparation.step(ctx, encoding)
		if err != nil {
			return docRangeHeaderHeapEstimate{}, err
		}
		if complete {
			return preparation.heap, nil
		}
	}
}

func (encoding *LegacyCanonicalEncoding) inspectContentBlock(ctx context.Context, block ContentBlock, depth int, prepare bool) (docRangeHeaderHeapEstimate, error) {
	var heap docRangeHeaderHeapEstimate
	if err := ctx.Err(); err != nil {
		return heap, err
	}
	if depth >= 10000 {
		return heap, fmt.Errorf("%w: legacy canonical container depth", ErrValidation)
	}
	heap.allocation(int64(len(block.Text)), 1)
	heap.allocation(int64(len(block.Type)), 1)
	return heap, heap.err
}

func (encoding *LegacyCanonicalEncoding) Next(ctx context.Context) ([]byte, bool, error) {
	if encoding != nil {
		encoding.lastStorageBytes = 0
	}
	if err := ctx.Err(); err != nil {
		if encoding != nil && encoding.initialized {
			return encoding.fail(err)
		}
		return nil, false, err
	}
	if encoding == nil || !encoding.initialized {
		return nil, false, ErrValidation
	}
	if encoding.err != nil {
		return nil, false, encoding.err
	}
	if encoding.complete {
		return nil, true, nil
	}
	if encoding.preparation != nil {
		preparation := encoding.preparation
		complete, err := preparation.step(ctx, encoding)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return encoding.fail(ctxErr)
		}
		if err != nil {
			return encoding.fail(err)
		}
		if complete {
			bytes, err := encoding.frames.appendBytes()
			if err != nil {
				return encoding.fail(err)
			}
			if preparation.lastActions == legacyCanonicalEncodingActions || !preparation.reserve(bytes+int(unsafe.Sizeof(legacyCanonicalEncodingFrame{}))) {
				encoding.lastStorageBytes = preparation.lastStorageBytes
				return encoding.finishPreparation(ctx)
			}
			if err := encoding.frames.append(legacyCanonicalEncodingFrame{value: encoding.root}); err != nil {
				return encoding.fail(err)
			}
			preparation.lastActions++
			preparation.lastStorageBytes += bytes
			encoding.inspectWork++
			encoding.preparation = nil
		}
		encoding.lastStorageBytes = preparation.lastStorageBytes
		return encoding.finishPreparation(ctx)
	}
	if encoding.numbers.next < encoding.numbers.pending.length() {
		_, err := encoding.numbers.step(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return encoding.fail(ctxErr)
		}
		if err != nil {
			return encoding.fail(err)
		}
		return nil, false, nil
	}
	var output []byte
	sourceBytes := 0
	lastChecked := 0
	for actions := 0; actions < legacyCanonicalEncodingActions && len(output) < LegacyCanonicalEncodingBytes && encoding.frames.length() != 0; actions++ {
		if err := ctx.Err(); err != nil {
			return encoding.fail(err)
		}
		frame := encoding.frames.at(encoding.frames.length() - 1)
		if frame.kind == 'a' || frame.kind == 'm' || frame.kind == 'b' || frame.kind == 'c' || frame.kind == 'd' || frame.kind == 'e' || frame.kind == 'r' || frame.kind == 'o' && frame.ordered ||
			frame.kind == 's' && (frame.phase == 0 || frame.position == len(frame.text)) {
			output = legacyCanonicalGrow(output, 1)
		}
		switch frame.kind {
		case 0:
			numbered := false
			switch value := frame.value.(type) {
			case json.Number:
				numbered = true
			case *json.Number:
				numbered = value != nil
			}
			if numbered {
				if legacyCanonicalNumberProofBytes > LegacyCanonicalEncodingBytes-sourceBytes {
					return encoding.finishNext(ctx, output)
				}
				sourceBytes += legacyCanonicalNumberProofBytes
			}
			if err := encoding.introduce(frame); err != nil {
				return encoding.fail(err)
			}
		case 's':
			if frame.phase == 0 {
				output = append(output, '"')
				frame.phase = 1
			} else if frame.position < len(frame.text) {
				remaining := len(frame.text) - frame.position
				if remaining > 1024 {
					output = legacyCanonicalGrow(output, min(remaining, LegacyCanonicalEncodingBytes-len(output)))
				}
				for frame.position < len(frame.text) {
					if sourceBytes == LegacyCanonicalEncodingBytes {
						break
					}
					start := frame.position
					limit := start + min(len(frame.text)-start, LegacyCanonicalEncodingBytes-sourceBytes, LegacyCanonicalEncodingBytes-len(output))
					for frame.position < limit {
						character := frame.text[frame.position]
						if character < 0x20 || character >= utf8.RuneSelf || character == '"' || character == '\\' || character == '<' || character == '>' || character == '&' {
							break
						}
						frame.position++
					}
					if frame.position > start {
						length := frame.position - start
						output = legacyCanonicalGrow(output, length)
						output = append(output, frame.text[start:frame.position]...)
						sourceBytes += length
						if sourceBytes-lastChecked >= canonicalCheckBytes {
							if err := ctx.Err(); err != nil {
								return encoding.fail(err)
							}
							lastChecked = sourceBytes
						}
						continue
					}
					if frame.text[frame.position] >= utf8.RuneSelf && LegacyCanonicalEncodingBytes-sourceBytes < utf8.UTFMax {
						break
					}
					unit, width, consumed := legacyCanonicalStringUnit(frame.text[frame.position:])
					if consumed > LegacyCanonicalEncodingBytes-sourceBytes || width > LegacyCanonicalEncodingBytes-len(output) {
						break
					}
					output = legacyCanonicalGrow(output, width)
					output = append(output, unit[:width]...)
					frame.position += consumed
					sourceBytes += consumed
					if sourceBytes-lastChecked >= canonicalCheckBytes {
						if err := ctx.Err(); err != nil {
							return encoding.fail(err)
						}
						lastChecked = sourceBytes
					}
				}
				if frame.position < len(frame.text) {
					return encoding.finishNext(ctx, output)
				}
			} else {
				output = append(output, '"')
				encoding.pop()
			}
		case 'l':
			width := min(len(frame.text)-frame.position, LegacyCanonicalEncodingBytes-len(output), LegacyCanonicalEncodingBytes-sourceBytes)
			output = legacyCanonicalGrow(output, width)
			output = append(output, frame.text[frame.position:frame.position+width]...)
			frame.position += width
			sourceBytes += width
			if frame.position == len(frame.text) {
				encoding.pop()
			} else {
				return encoding.finishNext(ctx, output)
			}
		case 'o':
			if !frame.ordered {
				if !encoding.ordering {
					return encoding.fail(ErrIntegrity)
				}
				before := encoding.order.work
				used, complete, yield, err := encoding.order.advance(&frame.fields, LegacyCanonicalEncodingBytes-sourceBytes)
				encoding.keyWork += encoding.order.work - before
				if err != nil || used < 0 || used > LegacyCanonicalEncodingBytes-sourceBytes {
					if err == nil {
						err = ErrIntegrity
					}
					return encoding.fail(err)
				}
				sourceBytes += used
				frame.ordered = complete
				encoding.ordering = !complete
				if yield {
					return encoding.finishNext(ctx, output)
				}
				continue
			}
			if frame.phase == 0 {
				output = append(output, '{')
				frame.phase = 1
			} else if frame.phase == 2 {
				ready, err := encoding.reserveFrame(&sourceBytes)
				if err != nil {
					return encoding.fail(err)
				}
				if !ready {
					return encoding.finishNext(ctx, output)
				}
				output = append(output, ':')
				child := *frame.fields.at(frame.position)
				frame.position++
				frame.phase = 1
				if err := encoding.frames.append(legacyCanonicalEncodingFrame{value: child.value, keysPosition: child.childKeys}); err != nil {
					return encoding.fail(err)
				}
			} else if frame.position == frame.fields.length() {
				output = append(output, '}')
				encoding.pop()
			} else {
				ready, err := encoding.reserveFrame(&sourceBytes)
				if err != nil {
					return encoding.fail(err)
				}
				if !ready {
					return encoding.finishNext(ctx, output)
				}
				if frame.position != 0 {
					output = append(output, ',')
				}
				key := frame.fields.at(frame.position).key
				frame.phase = 2
				if err := encoding.frames.append(legacyCanonicalEncodingFrame{kind: 's', text: key, keysPosition: frame.keysPosition}); err != nil {
					return encoding.fail(err)
				}
			}
		case 'b':
			if frame.phase == 0 {
				output = append(output, '{')
				frame.phase = 1
			} else if frame.phase == 2 {
				ready, err := encoding.reserveFrame(&sourceBytes)
				if err != nil {
					return encoding.fail(err)
				}
				if !ready {
					return encoding.finishNext(ctx, output)
				}
				output = append(output, ':')
				text := frame.block.Text
				if frame.position != 0 {
					text = frame.block.Type
				}
				frame.position++
				frame.phase = 1
				if err := encoding.frames.append(legacyCanonicalEncodingFrame{kind: 's', text: text, keysPosition: frame.keysPosition}); err != nil {
					return encoding.fail(err)
				}
			} else if frame.position == 2 {
				output = append(output, '}')
				encoding.pop()
			} else {
				ready, err := encoding.reserveFrame(&sourceBytes)
				if err != nil {
					return encoding.fail(err)
				}
				if !ready {
					return encoding.finishNext(ctx, output)
				}
				key := "text"
				if frame.position != 0 {
					output = append(output, ',')
					key = "type"
				}
				frame.phase = 2
				if err := encoding.frames.append(legacyCanonicalEncodingFrame{kind: 's', text: key, keysPosition: frame.keysPosition}); err != nil {
					return encoding.fail(err)
				}
			}
		case 'a', 'm', 'c', 'd', 'e', 'r':
			if frame.phase == 0 {
				output = append(output, '[')
				frame.phase = 1
			} else {
				var child any
				var length int
				childKeys := frame.keysPosition
				if frame.kind == 'e' {
					values, valid := frame.value.(*legacyCanonicalPreparedArray)
					if !valid || values == nil {
						return encoding.fail(ErrIntegrity)
					}
					length = values.values.length()
					if frame.position < length {
						value := values.values.at(frame.position)
						if value == nil {
							return encoding.fail(ErrIntegrity)
						}
						child = *value
					}
				} else if frame.kind == 'a' {
					values := frame.value.([]any)
					length = len(values)
					if frame.position < length {
						child = values[frame.position]
					}
				} else if frame.kind == 'm' {
					values := frame.value.([]map[string]any)
					length = len(values)
					if frame.position < length {
						child = values[frame.position]
					}
				} else if frame.kind == 'r' {
					length = frame.fields.length()
					if frame.position < length {
						field := frame.fields.at(frame.position)
						child, childKeys = field.value, field.childKeys
					}
				} else if frame.kind == 'd' {
					length = frame.value.(*legacyNativeBlockValues).count
				} else {
					length = len(frame.value.([]ContentBlock))
				}
				if frame.position == length {
					output = append(output, ']')
					encoding.pop()
				} else {
					ready, err := encoding.reserveFrame(&sourceBytes)
					if err != nil {
						return encoding.fail(err)
					}
					if !ready {
						return encoding.finishNext(ctx, output)
					}
					if frame.position != 0 {
						output = append(output, ',')
					}
					frame.position++
					if frame.kind == 'c' || frame.kind == 'd' {
						var block ContentBlock
						if frame.kind == 'c' {
							block = frame.value.([]ContentBlock)[frame.position-1]
						} else {
							record := frame.value.(*legacyNativeBlockValues).records.at(frame.position - 1)
							if record == nil {
								return encoding.fail(ErrIntegrity)
							}
							block = record.block
						}
						if err := encoding.frames.append(legacyCanonicalEncodingFrame{kind: 'b', block: block, keysPosition: frame.keysPosition}); err != nil {
							return encoding.fail(err)
						}
					} else {
						if err := encoding.frames.append(legacyCanonicalEncodingFrame{value: child, keysPosition: childKeys}); err != nil {
							return encoding.fail(err)
						}
					}
				}
			}
		default:
			return encoding.fail(ErrIntegrity)
		}
	}
	return encoding.finishNext(ctx, output)
}

func (encoding *LegacyCanonicalEncoding) reserveFrame(sourceBytes *int) (bool, error) {
	bytes, err := encoding.frames.appendBytes()
	if err != nil {
		return false, err
	}
	work := bytes + int(unsafe.Sizeof(legacyCanonicalEncodingFrame{}))
	if work > LegacyCanonicalEncodingBytes-*sourceBytes {
		return false, nil
	}
	*sourceBytes += work
	encoding.lastStorageBytes += bytes
	return true, nil
}

func legacyCanonicalGrow(output []byte, additional int) []byte {
	if additional <= cap(output)-len(output) {
		return output
	}
	capacity := min(LegacyCanonicalEncodingBytes, max(len(output)+additional, 2*cap(output), 256))
	grown := make([]byte, len(output), capacity)
	copy(grown, output)
	return grown
}

func (encoding *LegacyCanonicalEncoding) introduce(frame *legacyCanonicalEncodingFrame) error {
	switch value := frame.value.(type) {
	case string:
		frame.kind, frame.text = 's', value
	case EventKind:
		frame.kind, frame.text = 's', string(value)
	case Role:
		frame.kind, frame.text = 's', string(value)
	case ProviderKind:
		frame.kind, frame.text = 's', string(value)
	case FidelityTier:
		frame.kind, frame.text = 's', string(value)
	case json.Number:
		if !encoding.numbers.valid(string(value)) {
			return ErrIntegrity
		}
		frame.kind, frame.text = 'l', string(value)
		if frame.text == "" {
			frame.text = "0"
		}
	case *json.Number:
		frame.kind, frame.text = 'l', "null"
		if value != nil {
			if !encoding.numbers.valid(string(*value)) {
				return ErrIntegrity
			}
			frame.text = string(*value)
			if frame.text == "" {
				frame.text = "0"
			}
		}
	case ContentBlock:
		frame.kind, frame.block = 'b', value
	case *legacyCanonicalReplacement:
		if value == nil || !encoding.claimKeys(frame, value.length()) || !frame.ordered {
			return ErrIntegrity
		}
		frame.kind = 'r'
	case *legacyCanonicalPreparedReplacement:
		if value == nil || value.count < 0 || !encoding.claimKeys(frame, value.count) || !frame.ordered {
			return ErrIntegrity
		}
		frame.kind = 'r'
	case *legacyCanonicalPreparedObject:
		if value == nil || value.count < 0 || !encoding.claimKeys(frame, value.count) {
			return ErrIntegrity
		}
		frame.kind = 'o'
		if !frame.ordered {
			if encoding.ordering {
				return ErrIntegrity
			}
			encoding.order = newLegacyCanonicalKeyOrder(frame.fields.length(), false)
			encoding.ordering = true
		}
	case *legacyCanonicalPreparedArray:
		if value == nil || !value.values.valid() {
			return ErrIntegrity
		}
		frame.kind = 'e'
	case map[string]any:
		if value == nil {
			frame.kind, frame.text = 'l', "null"
			break
		}
		if !encoding.claimKeys(frame, len(value)) {
			return ErrIntegrity
		}
		frame.kind = 'o'
		if !frame.ordered {
			if encoding.ordering {
				return ErrIntegrity
			}
			encoding.order = newLegacyCanonicalKeyOrder(frame.fields.length(), false)
			encoding.ordering = true
		}
	case []any:
		if value == nil {
			frame.kind, frame.text = 'l', "null"
		} else {
			frame.kind = 'a'
		}
	case []map[string]any:
		if value == nil {
			frame.kind, frame.text = 'l', "null"
		} else {
			frame.kind = 'm'
		}
	case []ContentBlock:
		if value == nil {
			frame.kind, frame.text = 'l', "null"
		} else {
			frame.kind = 'c'
		}
	case *legacyNativeBlockValues:
		if value == nil {
			frame.kind, frame.text = 'l', "null"
		} else {
			frame.kind = 'd'
		}
	case nil, bool, int, float64:
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		frame.kind, frame.text, frame.ownedLiteral = 'l', string(raw), true
	default:
		return ErrValidation
	}
	return nil
}

func (encoding *LegacyCanonicalEncoding) claimKeys(frame *legacyCanonicalEncodingFrame, count int) bool {
	if frame.keysPosition < 0 || frame.keysPosition >= encoding.mapKeys.length() {
		return false
	}
	entry := encoding.mapKeys.at(frame.keysPosition)
	if entry == nil || entry.endKeys <= frame.keysPosition || entry.endKeys > encoding.mapKeys.length() || entry.fields.length() != count ||
		entry.heap < 0 || entry.heap > encoding.pendingKeysHeap.bytes {
		return false
	}
	frame.fields, frame.keysEnd, frame.ordered = entry.fields, entry.endKeys, entry.sorted
	encoding.pendingKeysHeap.bytes -= entry.heap
	*entry = legacyCanonicalEncodingKeys{}
	return true
}

func legacyCanonicalStringUnit(text string) (unit [6]byte, width, consumed int) {
	character := text[0]
	if character < utf8.RuneSelf {
		consumed = 1
		switch character {
		case '"', '\\':
			unit[0], unit[1] = '\\', character
			return unit, 2, consumed
		case '\b', '\f', '\n', '\r', '\t':
			unit[0] = '\\'
			switch character {
			case '\b':
				unit[1] = 'b'
			case '\f':
				unit[1] = 'f'
			case '\n':
				unit[1] = 'n'
			case '\r':
				unit[1] = 'r'
			case '\t':
				unit[1] = 't'
			}
			return unit, 2, consumed
		}
		if character < 0x20 || character == '<' || character == '>' || character == '&' {
			const hex = "0123456789abcdef"
			unit = [6]byte{'\\', 'u', '0', '0', hex[character>>4], hex[character&15]}
			return unit, 6, consumed
		}
		unit[0] = character
		return unit, 1, consumed
	}
	decoded, consumed := utf8.DecodeRuneInString(text)
	if decoded == utf8.RuneError && consumed == 1 {
		unit = [6]byte{'\\', 'u', 'f', 'f', 'f', 'd'}
		return unit, 6, consumed
	}
	if decoded == '\u2028' || decoded == '\u2029' {
		unit = [6]byte{'\\', 'u', '2', '0', '2', byte('8' + decoded - '\u2028')}
		return unit, 6, consumed
	}
	copy(unit[:], text[:consumed])
	return unit, consumed, consumed
}

func (encoding *LegacyCanonicalEncoding) pop() {
	last := encoding.frames.length() - 1
	frame := encoding.frames.at(last)
	keysPosition := frame.keysPosition
	if frame.kind == 'o' || frame.kind == 'r' {
		keysPosition = frame.keysEnd
	}
	encoding.frames.pop()
	if last > 0 {
		encoding.frames.at(last - 1).keysPosition = keysPosition
	}
}

func (encoding *LegacyCanonicalEncoding) finishNext(ctx context.Context, output []byte) ([]byte, bool, error) {
	if encoding.frames.length() == 0 {
		encoding.release()
		encoding.complete = true
	}
	if err := ctx.Err(); err != nil {
		return encoding.fail(err)
	}
	return output, encoding.complete, nil
}

func (encoding *LegacyCanonicalEncoding) fail(err error) ([]byte, bool, error) {
	encoding.release()
	encoding.err = err
	return nil, false, err
}

func (encoding *LegacyCanonicalEncoding) release() {
	if encoding.nativeOwner != nil {
		encoding.nativeOwner.release()
	}
	encoding.nativeOwner = nil
	encoding.preparation = nil
	encoding.root = nil
	encoding.rootHeap = docRangeHeaderHeapEstimate{}
	encoding.mapKeys.clear()
	encoding.pendingKeysHeap = docRangeHeaderHeapEstimate{}
	encoding.frames.clear()
	encoding.order = legacyCanonicalKeyOrder{}
	encoding.ordering = false
	encoding.numbers = legacyCanonicalNumberValidation{}
}

func (encoding *LegacyCanonicalEncoding) RetainedBytes() (int64, error) {
	if encoding == nil || !encoding.initialized {
		return 0, ErrValidation
	}
	if encoding.err != nil {
		return 0, encoding.err
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*encoding)))
	heap.include(encoding.rootHeap)
	heap.include(encoding.pendingKeysHeap)
	if encoding.nativeOwner != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*encoding.nativeOwner)))
	}
	if encoding.preparation != nil {
		encoding.preparation.retained(&heap)
	}
	encoding.numbers.retained(&heap)
	encoding.mapKeys.retained(&heap)
	encoding.frames.retained(&heap)
	for position := 0; position < encoding.frames.length(); position++ {
		frame := encoding.frames.at(position)
		frame.fields.retained(&heap)
		if frame.ownedLiteral {
			heap.allocation(int64(len(frame.text)), 1)
		}
	}
	if heap.err != nil {
		return 0, heap.err
	}
	return heap.bytes, nil
}
