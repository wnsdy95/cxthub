package domain

import (
	"context"
	"errors"
	"unsafe"
)

const (
	legacyNativeStringsStart uint8 = iota
	legacyNativeStringsFirst
	legacyNativeStringsValue
	legacyNativeStringsString
	legacyNativeStringsApply
	legacyNativeStringsNull
	legacyNativeStringsAfter
	legacyNativeStringsTail
	legacyNativeStringsDone
)

type legacyNativeStringList struct {
	decoded       []string
	previous      []string
	capacities    []int
	previousCaps  []int
	estimate      docRangeHeaderHeapEstimate
	payload       docRangeHeaderHeapEstimate
	added         docRangeHeaderHeapEstimate
	scalar        *legacyNativeScalar
	position      int
	end           int
	maximum       int
	count         int
	copied        int
	stringStart   int
	literal       int
	seeded        int
	phase         uint8
	escaped       bool
	nullList      bool
	owned         bool
	previousOwned bool
	initialized   bool
	prefix        bool
}

func newLegacyNativeStringList(prior []string, priorHeap docRangeHeaderHeapEstimate, from, to int) (*legacyNativeStringList, error) {
	if from < 0 || to <= from || priorHeap.bytes < 0 {
		return nil, ErrIntegrity
	}
	if priorHeap.err != nil {
		return nil, priorHeap.err
	}
	var minimum docRangeHeaderHeapEstimate
	minimum.allocation(1, int64(unsafe.Sizeof(prior)))
	minimum.include(legacyNativeStringsBacking(cap(prior)))
	if minimum.err != nil {
		return nil, minimum.err
	}
	if priorHeap.bytes == 0 && cap(prior) == 0 {
		priorHeap = minimum
	}
	if priorHeap.bytes < minimum.bytes {
		return nil, ErrIntegrity
	}
	if cap(prior) == 0 && priorHeap.bytes != minimum.bytes {
		return nil, ErrIntegrity
	}
	list := &legacyNativeStringList{
		decoded:     prior,
		estimate:    priorHeap,
		phase:       legacyNativeStringsDone,
		initialized: cap(prior) == 0,
	}
	if err := list.start(from, to); err != nil {
		return nil, err
	}
	return list, nil
}

func (list *legacyNativeStringList) start(from, to int) error {
	if list == nil || list.phase != legacyNativeStringsDone || from < 0 || to <= from {
		return ErrIntegrity
	}
	list.decoded = list.decoded[:cap(list.decoded)]
	list.position, list.end, list.maximum = from, to, (to-from)/3
	list.count, list.copied, list.stringStart, list.literal = 0, 0, 0, 0
	list.escaped, list.nullList, list.owned = false, false, false
	list.prefix = false
	list.added = docRangeHeaderHeapEstimate{}
	list.phase = legacyNativeStringsStart
	return nil
}

func legacyNativeStringsBacking(capacity int) docRangeHeaderHeapEstimate {
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(int64(capacity), int64(unsafe.Sizeof(string(""))))
	return estimate
}

func (list *legacyNativeStringList) seed(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	const width = int(unsafe.Sizeof(string("")) + unsafe.Sizeof(int(0)))
	if list.capacities == nil {
		list.capacities = make([]int, cap(list.decoded))
	}
	for list.seeded < len(list.decoded) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if ConversationManifestChunkBytes-input.workBytes < width {
			return false, nil
		}
		capacity := len(list.decoded[list.seeded])
		list.capacities[list.seeded] = capacity
		list.payload.allocation(int64(capacity), 1)
		if list.payload.err != nil {
			return false, list.payload.err
		}
		input.workBytes += width
		list.seeded++
	}
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(list.decoded)))
	estimate.include(legacyNativeStringsBacking(cap(list.decoded)))
	estimate.include(list.payload)
	if estimate.err != nil {
		return false, estimate.err
	}
	if estimate.bytes != list.estimate.bytes {
		return false, ErrIntegrity
	}
	list.initialized = true
	return true, nil
}

func (list *legacyNativeStringList) reserve(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	const width = int(unsafe.Sizeof(string("")) + unsafe.Sizeof(int(0)))
	if list.previous == nil && list.count >= cap(list.decoded) {
		if list.count >= list.maximum {
			return false, ErrIntegrity
		}
		if ConversationManifestChunkBytes-input.workBytes < 2*width {
			return false, nil
		}
		capacity := list.maximum
		if cap(list.decoded) <= list.maximum/2 {
			capacity = max(list.count+1, 2*cap(list.decoded))
		}
		oldBacking := legacyNativeStringsBacking(cap(list.decoded))
		nextHeap := list.estimate
		if oldBacking.err != nil || nextHeap.bytes < oldBacking.bytes {
			return false, ErrIntegrity
		}
		nextHeap.bytes -= oldBacking.bytes
		nextHeap.include(legacyNativeStringsBacking(capacity))
		if nextHeap.err != nil {
			return false, nextHeap.err
		}
		next := make([]string, capacity)
		nextCaps := make([]int, capacity)
		list.previous, list.previousOwned = list.decoded, list.owned
		list.previousCaps, list.capacities = list.capacities, nextCaps
		list.decoded, list.owned, list.copied, list.estimate = next, true, 0, nextHeap
		if len(list.previous) == 0 {
			list.previous, list.previousCaps, list.previousOwned = nil, nil, false
		}
	}
	for list.previous != nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		length := min(len(list.previous)-list.copied, canonicalCheckBytes/(2*width), (ConversationManifestChunkBytes-input.workBytes)/(2*width))
		if length <= 0 {
			return false, nil
		}
		copy(list.decoded[list.copied:list.copied+length], list.previous[list.copied:list.copied+length])
		copy(list.capacities[list.copied:list.copied+length], list.previousCaps[list.copied:list.copied+length])
		list.copied += length
		input.workBytes += 2 * width * length
		if list.copied == len(list.previous) {
			list.previous, list.previousOwned = nil, false
			list.previousCaps = nil
		}
	}
	return true, nil
}

func (list *legacyNativeStringList) finishArray() {
	if list.count == 0 {
		list.decoded, list.owned = make([]string, 0), false
		list.capacities, list.payload = nil, docRangeHeaderHeapEstimate{}
		list.estimate = docRangeHeaderHeapEstimate{}
		list.estimate.allocation(1, int64(unsafe.Sizeof(list.decoded)))
	} else {
		list.decoded = list.decoded[:list.count]
	}
	list.phase = legacyNativeStringsTail
}

func (list *legacyNativeStringList) step(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if list == nil || input == nil || list.end > input.source.Bytes {
		return false, ErrIntegrity
	}
	if !list.initialized {
		ready, err := list.seed(ctx, input)
		if err != nil || !ready {
			return false, err
		}
	}
	if list.phase == legacyNativeStringsDone {
		return true, nil
	}
	for input.workBytes < ConversationManifestChunkBytes {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if list.phase == legacyNativeStringsDone {
			return true, nil
		}
		if list.phase == legacyNativeStringsApply {
			if list.scalar != nil {
				complete, err := list.scalar.step(ctx, input)
				if err != nil || !complete {
					return false, err
				}
			}
			const width = int(unsafe.Sizeof(string("")) + unsafe.Sizeof(int(0)))
			if ConversationManifestChunkBytes-input.workBytes < width {
				return false, nil
			}
			if list.scalar != nil {
				var old, next docRangeHeaderHeapEstimate
				old.allocation(int64(list.capacities[list.count]), 1)
				next.allocation(int64(list.scalar.text.Cap()), 1)
				if old.err != nil || list.estimate.bytes < old.bytes || list.payload.bytes < old.bytes {
					return false, ErrIntegrity
				}
				list.estimate.bytes -= old.bytes
				list.estimate.include(next)
				list.payload.bytes -= old.bytes
				list.payload.include(next)
				list.added.include(next)
				if list.estimate.err != nil || list.payload.err != nil || list.added.err != nil {
					return false, errors.Join(list.estimate.err, list.payload.err, list.added.err)
				}
				list.decoded[list.count] = list.scalar.text.String()
				list.capacities[list.count] = list.scalar.text.Cap()
				list.scalar = nil
			}
			input.workBytes += width
			list.count++
			list.phase = legacyNativeStringsAfter
			continue
		}
		if list.phase == legacyNativeStringsTail && (list.prefix || list.position == list.end) {
			list.phase = legacyNativeStringsDone
			list.added, list.owned = docRangeHeaderHeapEstimate{}, false
			return true, nil
		}
		if list.position >= list.end {
			return false, ErrIntegrity
		}
		current, err := input.current(list.position)
		if errors.Is(err, errLegacyNormalizationYield) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		input.semanticBytes++
		switch list.phase {
		case legacyNativeStringsStart:
			switch {
			case legacyNormalizationSpace(current):
				list.position++
			case current == '[':
				list.position++
				list.phase = legacyNativeStringsFirst
			case current == 'n':
				list.nullList, list.literal, list.phase = true, 0, legacyNativeStringsNull
			default:
				return false, ErrIntegrity
			}
		case legacyNativeStringsFirst, legacyNativeStringsValue:
			if legacyNormalizationSpace(current) {
				list.position++
				continue
			}
			if current == ']' && list.phase == legacyNativeStringsFirst {
				list.position++
				list.finishArray()
				continue
			}
			if current != '"' && current != 'n' {
				return false, ErrIntegrity
			}
			ready, err := list.reserve(ctx, input)
			if err != nil || !ready {
				return false, err
			}
			if current == '"' {
				list.position++
				list.stringStart, list.escaped, list.phase = list.position, false, legacyNativeStringsString
			} else {
				list.literal, list.phase = 0, legacyNativeStringsNull
			}
		case legacyNativeStringsString:
			list.position++
			if list.escaped {
				list.escaped = false
			} else if current == '\\' {
				list.escaped = true
			} else if current == '"' {
				length := list.position - list.stringStart - 1
				if length > int(^uint(0)>>1)/3 {
					return false, ErrIntegrity
				}
				list.scalar = &legacyNativeScalar{position: list.stringStart, end: list.position - 1, maximum: 3 * length}
				list.phase = legacyNativeStringsApply
			}
		case legacyNativeStringsNull:
			if current != "null"[list.literal] {
				return false, ErrIntegrity
			}
			list.position++
			list.literal++
			if list.literal == len("null") {
				if list.nullList {
					list.decoded = nil
					list.capacities, list.payload = nil, docRangeHeaderHeapEstimate{}
					list.estimate = docRangeHeaderHeapEstimate{}
					list.estimate.allocation(1, int64(unsafe.Sizeof(list.decoded)))
					list.phase = legacyNativeStringsTail
				} else {
					list.phase = legacyNativeStringsApply
				}
			}
		case legacyNativeStringsAfter:
			switch {
			case legacyNormalizationSpace(current):
				list.position++
			case current == ',':
				list.position++
				list.phase = legacyNativeStringsValue
			case current == ']':
				list.position++
				list.finishArray()
			default:
				return false, ErrIntegrity
			}
		case legacyNativeStringsTail:
			if !legacyNormalizationSpace(current) {
				return false, ErrIntegrity
			}
			list.position++
		default:
			return false, ErrIntegrity
		}
	}
	return false, ctx.Err()
}

func (list *legacyNativeStringList) values() []string {
	return list.decoded
}

func (list *legacyNativeStringList) heap() docRangeHeaderHeapEstimate {
	return list.estimate
}

func (list *legacyNativeStringList) retained(estimate *docRangeHeaderHeapEstimate) {
	if list == nil {
		return
	}
	estimate.allocation(1, int64(unsafe.Sizeof(*list)))
	estimate.allocation(int64(cap(list.capacities)), int64(unsafe.Sizeof(int(0))))
	estimate.allocation(int64(cap(list.previousCaps)), int64(unsafe.Sizeof(int(0))))
	estimate.include(list.added)
	if list.owned {
		estimate.include(legacyNativeStringsBacking(cap(list.decoded)))
	}
	if list.previousOwned {
		estimate.include(legacyNativeStringsBacking(cap(list.previous)))
	}
	if list.scalar != nil {
		list.scalar.retained(estimate)
	}
}
