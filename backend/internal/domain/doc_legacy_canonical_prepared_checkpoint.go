package domain

import (
	"context"
	"encoding/json"
	"strings"
	"unsafe"
)

type legacyCanonicalPreparedKeysState struct {
	Fields  legacyNativeCheckpointTree `json:"fields"`
	EndKeys int                        `json:"end_keys"`
	Sorted  bool                       `json:"sorted"`
}

type legacyCanonicalPreparedFrameState struct {
	Value        legacyNativeCheckpointRef  `json:"value"`
	Block        legacyNativeCheckpointRef  `json:"block"`
	Text         legacyNativeCheckpointRef  `json:"text"`
	Fields       legacyNativeCheckpointTree `json:"fields"`
	KeysPosition int                        `json:"keys_position"`
	KeysEnd      int                        `json:"keys_end"`
	Position     int                        `json:"position"`
	Phase        byte                       `json:"phase"`
	Kind         byte                       `json:"kind"`
	Ordered      bool                       `json:"ordered"`
}

type legacyCanonicalPreparedOrderState struct {
	Phase         legacyCanonicalKeyOrderPhase `json:"phase"`
	Count         int                          `json:"count"`
	Build         int                          `json:"build"`
	Limit         int                          `json:"limit"`
	Root          int                          `json:"root"`
	Candidate     int                          `json:"candidate"`
	CompareLeft   int                          `json:"compare_left"`
	CompareRight  int                          `json:"compare_right"`
	CompareOffset int                          `json:"compare_offset"`
	Work          uint64                       `json:"work"`
}

func legacyCanonicalPreparedOrder(order legacyCanonicalKeyOrder) legacyCanonicalPreparedOrderState {
	return legacyCanonicalPreparedOrderState{Phase: order.phase, Count: order.count, Build: order.build, Limit: order.limit,
		Root: order.root, Candidate: order.candidate, CompareLeft: order.compareLeft, CompareRight: order.compareRight,
		CompareOffset: order.compareOffset, Work: order.work}
}

func (state legacyCanonicalPreparedOrderState) value() legacyCanonicalKeyOrder {
	return legacyCanonicalKeyOrder{phase: state.Phase, count: state.Count, build: state.Build, limit: state.Limit,
		root: state.Root, candidate: state.Candidate, compareLeft: state.CompareLeft, compareRight: state.CompareRight,
		compareOffset: state.CompareOffset, work: state.Work}
}

type legacyCanonicalPreparedState struct {
	Kind         string                             `json:"kind"`
	Version      uint8                              `json:"version"`
	Binding      ContentHash                        `json:"binding"`
	Numbers      legacyCanonicalPreparedNumberState `json:"numbers"`
	Keys         legacyNativeCheckpointTree         `json:"keys"`
	Frames       legacyNativeCheckpointTree         `json:"frames"`
	Order        legacyCanonicalPreparedOrderState  `json:"order"`
	Ordering     bool                               `json:"ordering"`
	KeyWork      uint64                             `json:"key_work"`
	InspectWork  uint64                             `json:"inspect_work"`
	EagerNumbers bool                               `json:"eager_numbers"`
}

type legacyCanonicalPreparedFrameWrite struct {
	source  *legacyCanonicalEncodingFrame
	numbers *legacyCanonicalNumberValidation
	binding ContentHash
	store   legacyNativeCheckpointIO
	budget  int64
	state   legacyCanonicalPreparedFrameState
	fields  *legacyCanonicalPreparedFieldsWrite
	value   *legacyCanonicalPreparedValueWrite
	phase   uint8
	ready   bool
}

func (work *legacyCanonicalPreparedFrameWrite) retainedBytes() (int64, error) {
	if work == nil {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.state.Value.Hash)+len(work.state.Block.Hash)+len(work.state.Text.Hash)+len(work.state.Fields.Ref.Hash)), 1)
	if work.fields != nil {
		retained, err := work.fields.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.value != nil {
		retained, err := work.value.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyCanonicalPreparedFrameWrite) step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if work == nil || work.source == nil {
		return false, ErrIntegrity
	}
	if work.ready {
		return true, nil
	}
	retained, err := work.retainedBytes()
	if err != nil || retained+legacyNativeAnyCheckpointScratch > work.budget {
		return false, ErrDocumentResourceLimit
	}
	frame := work.source
	if work.phase == 0 {
		work.state = legacyCanonicalPreparedFrameState{KeysPosition: frame.keysPosition, KeysEnd: frame.keysEnd,
			Position: frame.position, Phase: frame.phase, Kind: frame.kind, Ordered: frame.ordered}
		keyAt, valueAt, keySkip := 0, 0, -1
		if frame.kind == 'o' && frame.ordered {
			keyAt, valueAt = frame.position, frame.position
			if frame.phase == 2 {
				keySkip = frame.position
			}
		} else if frame.kind == 'r' {
			keyAt, valueAt = frame.fields.length(), frame.position
		}
		work.fields = &legacyCanonicalPreparedFieldsWrite{source: &frame.fields, numbers: work.numbers, binding: work.binding,
			store: work.store, budget: work.budget - retained, keyAt: keyAt, valueAt: valueAt, keySkip: keySkip}
		work.phase++
		return false, nil
	}
	if work.phase == 1 {
		childBytes, err := work.fields.retainedBytes()
		if err != nil {
			return false, err
		}
		if childBytes < 0 || childBytes > retained {
			return false, ErrIntegrity
		}
		work.fields.budget = work.budget - retained + childBytes
		ready, err := work.fields.step(ctx)
		if err != nil || !ready {
			return false, err
		}
		work.state.Fields = work.fields.tree
		work.fields.close()
		work.fields, work.phase = nil, 2
		return false, nil
	}
	if work.phase == 2 || work.phase == 4 || work.phase == 6 {
		var value any
		save := true
		switch work.phase {
		case 2:
			if frame.kind == 0 {
				value = frame.value
			} else if frame.kind == 'a' || frame.kind == 'm' || frame.kind == 'c' || frame.kind == 'd' || frame.kind == 'e' {
				value = legacyCanonicalPreparedArraySlice{value: frame.value, position: frame.position}
			} else {
				save = false
			}
		case 4:
			if frame.kind == 's' || frame.kind == 'l' {
				if frame.position < 0 || frame.position > len(frame.text) {
					return false, ErrIntegrity
				}
				if frame.kind == 'l' && !frame.ownedLiteral {
					if _, err := work.numbers.index(frame.text); err == nil {
						value = json.Number(frame.text)
					}
				}
				if value == nil {
					value = frame.text[frame.position:]
					work.state.Position = 0
				}
			} else {
				save = false
			}
		case 6:
			if frame.kind == 'b' {
				block := frame.block
				if frame.position >= 1 {
					block.Text = ""
				}
				if frame.position >= 2 {
					block.Type = ""
				}
				value = block
			} else {
				save = false
			}
		}
		if !save {
			work.phase += 2
			return false, nil
		}
		work.value, err = newLegacyCanonicalPreparedValueWrite(ctx, value, work.numbers, work.binding, work.store, work.budget-retained)
		if err == nil {
			work.phase++
		}
		return false, err
	}
	if work.phase == 3 || work.phase == 5 || work.phase == 7 {
		childBytes, err := work.value.retainedBytes()
		if err != nil {
			return false, err
		}
		if childBytes < 0 || childBytes > retained {
			return false, ErrIntegrity
		}
		work.value.budget = work.budget - retained + childBytes
		ready, err := work.value.step(ctx)
		if err != nil || !ready {
			return false, err
		}
		ref, err := work.value.take(ctx)
		work.value = nil
		if err != nil {
			return false, err
		}
		switch work.phase {
		case 3:
			work.state.Value = ref
		case 5:
			work.state.Text = ref
		case 7:
			work.state.Block = ref
		}
		work.phase++
		return false, nil
	}
	work.ready = true
	return true, nil
}

func (work *legacyCanonicalPreparedFrameWrite) close() {
	if work != nil {
		if work.fields != nil {
			work.fields.close()
		}
		if work.value != nil {
			work.value.close()
		}
		*work = legacyCanonicalPreparedFrameWrite{}
	}
}

type legacyCanonicalPreparedCheckpoint struct {
	source  *LegacyCanonicalEncoding
	state   legacyCanonicalPreparedState
	store   legacyNativeCheckpointIO
	budget  int64
	numbers *legacyCanonicalPreparedNumbersCheckpoint
	keys    legacyNativeCheckpointListWriter[legacyCanonicalPreparedKeysState]
	frames  legacyNativeCheckpointListWriter[legacyCanonicalPreparedFrameState]
	fields  *legacyCanonicalPreparedFieldsWrite
	frame   *legacyCanonicalPreparedFrameWrite
	index   int
	phase   uint8
	ref     legacyNativeCheckpointRef
	ready   bool
}

func newLegacyCanonicalPreparedCheckpoint(ctx context.Context, source *LegacyCanonicalEncoding, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalPreparedCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil || !source.initialized || source.complete || source.err != nil || source.preparation != nil ||
		source.frames.length() == 0 || source.frames.length() > 10000 || !source.frames.valid() || !source.mapKeys.valid() ||
		ValidateContentHash(binding) != nil || store.Put == nil || budget <= legacyNativeAnyCheckpointScratch {
		return nil, ErrIntegrity
	}
	state := legacyCanonicalPreparedState{Kind: "legacy-canonical-prepared", Version: 1, Binding: ContentHash(strings.Clone(string(binding))),
		Order: legacyCanonicalPreparedOrder(source.order), Ordering: source.ordering, KeyWork: source.keyWork,
		InspectWork: source.inspectWork, EagerNumbers: source.eagerNumbers}
	return &legacyCanonicalPreparedCheckpoint{source: source, state: state, store: store, budget: budget}, nil
}

func (work *legacyCanonicalPreparedCheckpoint) retainedBytes() (int64, error) {
	if work == nil {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.state.Binding)+len(work.state.Numbers.Pending.Ref.Hash)+len(work.state.Keys.Ref.Hash)+len(work.state.Frames.Ref.Hash)+len(work.ref.Hash)), 1)
	for _, list := range []interface{ retainedBytes() (int64, error) }{&work.keys, &work.frames} {
		retained, err := list.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.numbers != nil {
		retained, err := work.numbers.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.fields != nil {
		retained, err := work.fields.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.frame != nil {
		retained, err := work.frame.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyCanonicalPreparedCheckpoint) step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if work == nil || work.source == nil || work.source.preparation != nil || work.source.err != nil || work.source.complete {
		return false, ErrConflict
	}
	if work.ready {
		return true, nil
	}
	retained, err := work.retainedBytes()
	if err != nil || retained+legacyNativeAnyCheckpointScratch > work.budget {
		return false, ErrDocumentResourceLimit
	}
	if work.phase == 0 {
		if work.numbers == nil {
			work.numbers, err = newLegacyCanonicalPreparedNumbersCheckpoint(ctx, &work.source.numbers, work.state.Binding, work.store, work.budget-retained)
			return false, err
		}
		childBytes, err := work.numbers.retainedBytes()
		if err != nil {
			return false, err
		}
		if childBytes < 0 || childBytes > retained {
			return false, ErrIntegrity
		}
		work.numbers.budget = work.budget - retained + childBytes
		ready, err := work.numbers.step(ctx)
		if err != nil || !ready {
			return false, err
		}
		work.state.Numbers, err = work.numbers.take(ctx)
		work.numbers = nil
		if err == nil {
			work.phase++
		}
		return false, err
	}
	if work.phase == 1 {
		if work.index == work.source.mapKeys.length() {
			work.state.Keys, err = work.keys.finish(ctx, work.store)
			if err == nil {
				work.phase, work.index = 2, 0
			}
			return false, err
		}
		entry := work.source.mapKeys.at(work.index)
		if work.fields == nil {
			work.fields = &legacyCanonicalPreparedFieldsWrite{source: &entry.fields, numbers: &work.source.numbers,
				binding: work.state.Binding, store: work.store, budget: work.budget - retained, keySkip: -1}
			return false, nil
		}
		childBytes, err := work.fields.retainedBytes()
		if err != nil {
			return false, err
		}
		if childBytes < 0 || childBytes > retained {
			return false, ErrIntegrity
		}
		work.fields.budget = work.budget - retained + childBytes
		ready, err := work.fields.step(ctx)
		if err != nil || !ready {
			return false, err
		}
		state := legacyCanonicalPreparedKeysState{Fields: work.fields.tree, EndKeys: entry.endKeys, Sorted: entry.sorted}
		if err := work.keys.append(ctx, work.store, state); err != nil {
			return false, err
		}
		work.fields.close()
		work.fields = nil
		work.index++
		return false, nil
	}
	if work.phase == 2 {
		if work.index == work.source.frames.length() {
			work.state.Frames, err = work.frames.finish(ctx, work.store)
			if err == nil {
				work.phase++
			}
			return false, err
		}
		if work.frame == nil {
			work.frame = &legacyCanonicalPreparedFrameWrite{source: work.source.frames.at(work.index), numbers: &work.source.numbers,
				binding: work.state.Binding, store: work.store, budget: work.budget - retained}
			return false, nil
		}
		childBytes, err := work.frame.retainedBytes()
		if err != nil {
			return false, err
		}
		if childBytes < 0 || childBytes > retained {
			return false, ErrIntegrity
		}
		work.frame.budget = work.budget - retained + childBytes
		ready, err := work.frame.step(ctx)
		if err != nil || !ready {
			return false, err
		}
		if err := work.frames.append(ctx, work.store, work.frame.state); err != nil {
			return false, err
		}
		work.frame.close()
		work.frame = nil
		work.index++
		return false, nil
	}
	work.ref, err = legacyNativeCheckpointWriteJSON(ctx, work.store, work.state)
	work.ready = err == nil
	return work.ready, err
}

func (work *legacyCanonicalPreparedCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
	if err := ctx.Err(); err != nil {
		return legacyNativeCheckpointRef{}, err
	}
	if work == nil || !work.ready || !work.ref.valid() {
		return legacyNativeCheckpointRef{}, ErrDocRangeIndexPending
	}
	ref := work.ref
	work.close()
	return ref, nil
}

func (work *legacyCanonicalPreparedCheckpoint) close() {
	if work != nil {
		if work.numbers != nil {
			work.numbers.close()
		}
		if work.fields != nil {
			work.fields.close()
		}
		if work.frame != nil {
			work.frame.close()
		}
		work.keys.close()
		work.frames.close()
		*work = legacyCanonicalPreparedCheckpoint{}
	}
}
