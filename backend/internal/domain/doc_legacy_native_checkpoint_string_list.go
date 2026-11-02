package domain

import (
	"context"
	"strings"
	"unsafe"
)

const legacyNativeCheckpointStringListMetadataBytes = 8192

func legacyNativeCheckpointStringListValidate(state legacyNativeCheckpointStringListState, trees bool) error {
	if state.Version != legacyNativeCheckpointVersion || state.SourceBytes <= 0 || state.SourceBytes > MaxFinalizedDocBytes ||
		state.Position < 0 || state.Position > state.End || state.End > state.SourceBytes || state.End <= 0 ||
		state.Maximum < 0 || state.Maximum > state.End/3 || state.Position < max(0, state.End-3*state.Maximum-2) ||
		state.Count < 0 || state.Count > state.Maximum || state.Count > state.Decoded.Capacity ||
		state.Copied < 0 || state.Copied > state.Count || state.StringStart < 0 || state.StringStart > state.Position ||
		state.Seeded < 0 || state.Seeded > MaxFinalizedDocBytes || state.Phase > legacyNativeStringsDone ||
		state.Literal < 0 || state.Literal > len("null") {
		return ErrIntegrity
	}
	if err := state.Decoded.validate(trees); err != nil {
		return err
	}
	if err := state.Previous.validate(trees); err != nil {
		return err
	}
	if !state.Initialized && (state.Phase != legacyNativeStringsStart || state.Decoded.Capacity == 0 || state.Seeded >= state.Decoded.Capacity) ||
		state.Decoded.Capacity > 0 && state.Seeded > state.Decoded.Capacity ||
		state.Escaped && state.Phase != legacyNativeStringsString ||
		state.NullList && state.Phase != legacyNativeStringsNull && state.Phase != legacyNativeStringsTail && state.Phase != legacyNativeStringsDone ||
		state.Phase != legacyNativeStringsNull && state.Literal != 0 && state.Literal != len("null") {
		return ErrIntegrity
	}
	if !state.Previous.Nil {
		if !state.Initialized || state.Phase != legacyNativeStringsValue || state.NullList || state.HasScalar ||
			state.Previous.Capacity == 0 || state.Previous.Length != state.Previous.Capacity ||
			state.Previous.Length != state.Count || state.Copied >= state.Previous.Length ||
			state.Decoded.Capacity <= state.Previous.Capacity || state.Decoded.Capacity > state.Maximum {
			return ErrIntegrity
		}
	}
	if state.Phase < legacyNativeStringsTail {
		if state.Decoded.Length != state.Decoded.Capacity || state.Position >= state.End && state.Phase != legacyNativeStringsApply {
			return ErrIntegrity
		}
	} else if !state.Initialized || !state.Previous.Nil || state.Decoded.Length != state.Count ||
		state.Count == 0 && state.Decoded.Capacity != 0 || state.Decoded.Nil != state.NullList ||
		state.NullList && (state.Count != 0 || state.Literal != len("null")) ||
		state.Phase == legacyNativeStringsDone && !state.Prefix && state.Position != state.End {
		return ErrIntegrity
	}
	switch state.Phase {
	case legacyNativeStringsStart, legacyNativeStringsFirst:
		if state.Count != 0 || state.Copied != 0 || state.StringStart != 0 || state.Literal != 0 || state.NullList {
			return ErrIntegrity
		}
	case legacyNativeStringsValue:
		if state.Count == 0 || state.NullList {
			return ErrIntegrity
		}
	case legacyNativeStringsString, legacyNativeStringsApply:
		if state.Count >= state.Decoded.Capacity || !state.Previous.Nil || state.NullList ||
			state.Phase == legacyNativeStringsString && state.StringStart == 0 ||
			state.Phase == legacyNativeStringsApply && !state.HasScalar && state.Literal != len("null") {
			return ErrIntegrity
		}
	case legacyNativeStringsNull:
		if state.Literal == len("null") || !state.Previous.Nil ||
			state.NullList && (state.Count != 0 || state.Copied != 0 || state.StringStart != 0) ||
			!state.NullList && state.Count >= state.Decoded.Capacity {
			return ErrIntegrity
		}
	case legacyNativeStringsAfter:
		if state.Count == 0 || state.NullList {
			return ErrIntegrity
		}
	}
	if state.HasScalar {
		scalar := state.Scalar.State
		if err := scalar.validate(state.SourceBytes); err != nil {
			return err
		}
		if state.Phase != legacyNativeStringsApply || state.StringStart == 0 || state.StringStart >= state.Position ||
			scalar.Position < state.StringStart || scalar.End != state.Position-1 || scalar.Maximum != 3*(scalar.End-state.StringStart) ||
			scalar.Bytes != state.Scalar.Text.Bytes || scalar.Bytes > 3*(scalar.Position-state.StringStart) ||
			scalar.Position-state.StringStart > 6*scalar.Bytes || !scalar.Expanded && scalar.Bytes > scalar.Position-state.StringStart ||
			!scalar.Present && (scalar.Position != state.StringStart || scalar.Expanded) ||
			trees && !legacyNativeCheckpointTypedNumberTextValid(state.Scalar.Text) {
			return ErrIntegrity
		}
	} else if state.Scalar != (legacyNativeCheckpointScalar{}) {
		return ErrIntegrity
	}
	return nil
}

func legacyNativeCheckpointStringListDescriptor(values []string) legacyNativeCheckpointStrings {
	return legacyNativeCheckpointStrings{Nil: values == nil, Length: len(values), Capacity: cap(values)}
}

func legacyNativeCheckpointStringListMigrationStep(ctx context.Context, decoded, previous []string, capacities, previousCaps []int, copied int, checked, offset *int) (bool, error) {
	if previous == nil {
		return true, ctx.Err()
	}
	decoded = decoded[:cap(decoded)]
	remaining := LegacyCanonicalEncodingBytes
	for actions := 0; actions < legacyNativeCheckpointPageItems && *checked < len(decoded); actions++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		ordinal := *checked
		if ordinal < copied {
			text, prior := decoded[ordinal], previous[ordinal]
			if len(text) != len(prior) || capacities[ordinal] != previousCaps[ordinal] {
				return false, ErrIntegrity
			}
			length := min(len(text)-*offset, remaining)
			if length < 0 || text[*offset:*offset+length] != prior[*offset:*offset+length] {
				return false, ErrIntegrity
			}
			*offset += length
			remaining -= length
			if *offset != len(text) {
				return false, nil
			}
		} else if decoded[ordinal] != "" || capacities[ordinal] != 0 {
			return false, ErrIntegrity
		}
		*checked++
		*offset = 0
		if remaining == 0 {
			break
		}
	}
	return *checked == len(decoded), ctx.Err()
}

type legacyNativeCheckpointStringListWriter struct {
	state         legacyNativeCheckpointStringListState
	list          *legacyNativeStringList
	io            legacyNativeCheckpointIO
	values        *legacyNativeCheckpointStringValuesWriter
	scalar        *legacyNativeCheckpointScalarWriter
	ref           legacyNativeCheckpointRef
	budget        int64
	checked       int
	compared      int
	migrationDone bool
	decodedDone   bool
	previousDone  bool
	scalarDone    bool
	ready         bool
	closed        bool
	failure       error
}

func newLegacyNativeCheckpointStringListWriter(ctx context.Context, list *legacyNativeStringList, storage legacyNativeCheckpointIO, sourceBytes int, heapBudget int64) (*legacyNativeCheckpointStringListWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if list == nil || storage.Put == nil || heapBudget <= 0 ||
		list.initialized && len(list.capacities) != cap(list.decoded) ||
		!list.initialized && (len(list.capacities) != 0 && len(list.capacities) != cap(list.decoded) || list.seeded > len(list.capacities)) ||
		len(list.previousCaps) != cap(list.previous) {
		return nil, ErrIntegrity
	}
	state := legacyNativeCheckpointStringListState{
		Version: legacyNativeCheckpointVersion, SourceBytes: sourceBytes,
		Position: list.position, End: list.end, Maximum: list.maximum, Count: list.count,
		Copied: list.copied, StringStart: list.stringStart, Literal: list.literal, Seeded: list.seeded,
		Phase: list.phase, Escaped: list.escaped, NullList: list.nullList, Initialized: list.initialized, Prefix: list.prefix,
		Decoded: legacyNativeCheckpointStringListDescriptor(list.decoded), Previous: legacyNativeCheckpointStringListDescriptor(list.previous),
		HasScalar: list.scalar != nil,
	}
	var err error
	state.Scalar, err = legacyNativeCheckpointTypedNumberScalarState(list.scalar)
	if err != nil {
		return nil, err
	}
	if err := legacyNativeCheckpointStringListValidate(state, false); err != nil {
		return nil, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointStringListWriter{})))
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	return &legacyNativeCheckpointStringListWriter{state: state, list: list, io: storage, budget: heapBudget}, nil
}

func (work *legacyNativeCheckpointStringListWriter) writeValues(ctx context.Context, previous bool) error {
	if work.values == nil {
		list := work.list
		values := list.decoded
		capacities := func(current context.Context, ordinal int) (int, error) {
			if err := current.Err(); err != nil {
				return 0, err
			}
			if ordinal < 0 || ordinal >= cap(list.decoded) {
				return 0, ErrIntegrity
			}
			if !list.initialized && ordinal >= list.seeded {
				return len(list.decoded[:cap(list.decoded)][ordinal]), nil
			}
			return list.capacities[ordinal], nil
		}
		if previous {
			values = list.previous
			capacities = func(current context.Context, ordinal int) (int, error) {
				if err := current.Err(); err != nil {
					return 0, err
				}
				if ordinal < 0 || ordinal >= len(list.previousCaps) {
					return 0, ErrIntegrity
				}
				return list.previousCaps[ordinal], nil
			}
		}
		retained, err := work.retainedBytes()
		if err != nil {
			return err
		}
		work.values, err = newLegacyNativeCheckpointStringValuesWriter(ctx, values, capacities, work.io, work.budget-retained)
		return err
	}
	ready, err := work.values.step(ctx)
	if err != nil || !ready {
		return err
	}
	saved, err := work.values.take(ctx)
	if err != nil {
		return err
	}
	work.values = nil
	if previous {
		work.state.Previous, work.previousDone = saved, true
	} else {
		work.state.Decoded, work.decodedDone = saved, true
	}
	return nil
}

func (work *legacyNativeCheckpointStringListWriter) step(ctx context.Context) (bool, error) {
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
	if work.ready {
		return true, nil
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	var err error
	switch {
	case !work.migrationDone:
		work.migrationDone, err = legacyNativeCheckpointStringListMigrationStep(ctx, work.list.decoded, work.list.previous,
			work.list.capacities, work.list.previousCaps, work.state.Copied, &work.checked, &work.compared)
	case !work.decodedDone:
		err = work.writeValues(ctx, false)
	case !work.previousDone:
		err = work.writeValues(ctx, true)
	case !work.scalarDone:
		if !work.state.HasScalar {
			work.scalarDone = true
		} else if work.scalar == nil {
			work.scalar, err = newLegacyNativeCheckpointScalarWriter(ctx, work.list.scalar, work.io)
		} else {
			var ready bool
			ready, err = work.scalar.step(ctx)
			if err == nil && ready {
				work.state.Scalar, err = work.scalar.take(ctx)
				work.scalar = nil
				work.scalarDone = err == nil
			}
		}
	default:
		if err = legacyNativeCheckpointStringListValidate(work.state, true); err == nil {
			work.ref, err = legacyNativeCheckpointWriteJSON(ctx, work.io, work.state)
		}
		if err == nil && (!work.ref.valid() || work.ref.Bytes > legacyNativeCheckpointStringListMetadataBytes) {
			err = ErrIntegrity
		}
		if err == nil {
			work.list, work.ready = nil, true
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = work.admit(0)
	}
	if err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointStringListWriter) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	for _, hash := range []ContentHash{work.ref.Hash, work.state.Decoded.Items.Ref.Hash, work.state.Previous.Items.Ref.Hash, work.state.Scalar.Text.Tree.Ref.Hash} {
		heap.allocation(int64(len(hash)), 1)
	}
	retained, err := work.values.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	retained, err = work.scalar.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointStringListWriter) admit(scratch int64) error {
	retained, err := work.retainedBytes()
	return legacyNativeCheckpointTypedNumberAdmit(retained, err, work.budget, scratch)
}

func (work *legacyNativeCheckpointStringListWriter) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
	if work == nil {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointRef{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, work.failure
	}
	if work.closed || !work.ready || !work.ref.valid() {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, work.failure
	}
	ref := work.ref
	work.close()
	return ref, nil
}

func (work *legacyNativeCheckpointStringListWriter) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointStringListWriter) close() {
	if work != nil {
		work.values.close()
		work.scalar.close()
		failure := work.failure
		*work = legacyNativeCheckpointStringListWriter{closed: true, failure: failure}
	}
}

type legacyNativeCheckpointStringListRestore struct {
	state         legacyNativeCheckpointStringListState
	io            legacyNativeCheckpointIO
	ref           legacyNativeCheckpointRef
	values        *legacyNativeCheckpointStringValuesRestore
	scalar        *legacyNativeCheckpointScalarRestore
	decoded       legacyNativeCheckpointStringValues
	previous      legacyNativeCheckpointStringValues
	list          *legacyNativeStringList
	budget        int64
	sourceBytes   int
	checked       int
	compared      int
	normalized    int
	loaded        bool
	decodedDone   bool
	previousDone  bool
	migrationDone bool
	ready         bool
	closed        bool
	failure       error
}

func newLegacyNativeCheckpointStringListRestore(ctx context.Context, ref legacyNativeCheckpointRef, storage legacyNativeCheckpointIO, sourceBytes int, heapBudget int64) (*legacyNativeCheckpointStringListRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ref.Hash) != len(contentHashPrefix)+64 || !ref.valid() || ref.Bytes > legacyNativeCheckpointStringListMetadataBytes ||
		storage.Read == nil || sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes || heapBudget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointStringListRestore{})))
	planned.allocation(int64(len(ref.Hash)), 1)
	planned.allocation(int64(16*ref.Bytes), 1)
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	ref.Hash = ContentHash(strings.Clone(string(ref.Hash)))
	return &legacyNativeCheckpointStringListRestore{io: storage, ref: ref, sourceBytes: sourceBytes, budget: heapBudget}, nil
}

func (work *legacyNativeCheckpointStringListRestore) restoreValues(ctx context.Context, previous bool) error {
	saved := work.state.Decoded
	if previous {
		saved = work.state.Previous
	}
	if work.values == nil {
		retained, err := work.retainedBytes()
		if err != nil {
			return err
		}
		work.values, err = newLegacyNativeCheckpointStringValuesRestore(ctx, saved, work.io, work.sourceBytes, work.budget-retained)
		return err
	}
	ready, err := work.values.step(ctx)
	if err != nil || !ready {
		return err
	}
	restored, err := work.values.take(ctx)
	if err != nil {
		return err
	}
	work.values = nil
	if previous {
		work.previous, work.previousDone = restored, true
	} else {
		work.decoded, work.decodedDone = restored, true
	}
	if (restored.Values == nil) != saved.Nil || len(restored.Values) != saved.Length || cap(restored.Values) != saved.Capacity ||
		len(restored.Capacities) != saved.Capacity || restored.Payload.err != nil || restored.Payload.bytes < 0 {
		return ErrIntegrity
	}
	return nil
}

func (work *legacyNativeCheckpointStringListRestore) startList() error {
	if err := work.admit(int64(unsafe.Sizeof(legacyNativeStringList{}))); err != nil {
		return err
	}
	state := work.state
	work.list = &legacyNativeStringList{
		decoded: work.decoded.Values, previous: work.previous.Values,
		capacities: work.decoded.Capacities, previousCaps: work.previous.Capacities,
		position: state.Position, end: state.End, maximum: state.Maximum, count: state.Count,
		copied: state.Copied, stringStart: state.StringStart, literal: state.Literal,
		seeded: state.Decoded.Capacity, phase: state.Phase, escaped: state.Escaped, nullList: state.NullList,
		initialized: true, prefix: state.Prefix,
		owned:         state.Phase != legacyNativeStringsDone && state.Decoded.Capacity > 0,
		previousOwned: !state.Previous.Nil,
	}
	return nil
}

func (work *legacyNativeCheckpointStringListRestore) normalize(ctx context.Context) error {
	list := work.list
	decoded := list.decoded[:cap(list.decoded)]
	limit := min(len(decoded), work.normalized+legacyNativeCheckpointPageItems)
	for work.normalized < limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		ordinal := work.normalized
		capacity := list.capacities[ordinal]
		if list.previous != nil {
			if ordinal < list.copied {
				var duplicate docRangeHeaderHeapEstimate
				duplicate.allocation(int64(capacity), 1)
				if duplicate.err != nil || work.decoded.Payload.bytes < duplicate.bytes {
					return ErrIntegrity
				}
				work.decoded.Payload.bytes -= duplicate.bytes
				decoded[ordinal] = list.previous[ordinal]
				list.capacities[ordinal] = list.previousCaps[ordinal]
			}
			if ordinal < len(list.previous) {
				capacity = list.previousCaps[ordinal]
			}
		}
		list.payload.allocation(int64(capacity), 1)
		if list.payload.err != nil {
			return list.payload.err
		}
		work.normalized++
	}
	return ctx.Err()
}

func (work *legacyNativeCheckpointStringListRestore) restoreScalar(ctx context.Context) error {
	if work.scalar == nil {
		retained, err := work.retainedBytes()
		if err != nil {
			return err
		}
		work.scalar, err = newLegacyNativeCheckpointScalarRestore(ctx, work.state.Scalar, work.io, work.sourceBytes, work.budget-retained)
		return err
	}
	ready, err := work.scalar.step(ctx)
	if err != nil || !ready {
		return err
	}
	work.list.scalar, err = work.scalar.take(ctx)
	if err == nil {
		work.scalar = nil
	}
	return err
}

func (work *legacyNativeCheckpointStringListRestore) step(ctx context.Context) (bool, error) {
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
	if work.ready {
		return true, nil
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	var err error
	switch {
	case !work.loaded:
		if err = work.admit(int64(16 * work.ref.Bytes)); err == nil {
			err = legacyNativeCheckpointReadJSON(ctx, work.io, work.ref, &work.state)
		}
		if err == nil {
			err = legacyNativeCheckpointStringListValidate(work.state, true)
		}
		if err == nil && work.state.SourceBytes != work.sourceBytes {
			err = ErrIntegrity
		}
		work.loaded = err == nil
	case !work.decodedDone:
		err = work.restoreValues(ctx, false)
	case !work.previousDone:
		err = work.restoreValues(ctx, true)
	case work.list == nil:
		err = work.startList()
	case !work.migrationDone:
		work.migrationDone, err = legacyNativeCheckpointStringListMigrationStep(ctx, work.list.decoded, work.list.previous,
			work.list.capacities, work.list.previousCaps, work.state.Copied, &work.checked, &work.compared)
	case work.normalized < cap(work.list.decoded):
		err = work.normalize(ctx)
	case work.state.HasScalar && work.list.scalar == nil:
		err = work.restoreScalar(ctx)
	default:
		list := work.list
		list.estimate.allocation(1, int64(unsafe.Sizeof(list.decoded)))
		list.estimate.include(legacyNativeStringsBacking(cap(list.decoded)))
		list.estimate.include(list.payload)
		if list.phase != legacyNativeStringsDone {
			list.added = list.payload
		}
		err = list.estimate.err
		work.ready = err == nil
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = work.admit(0)
	}
	if err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointStringListRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	for _, hash := range []ContentHash{work.ref.Hash, work.state.Decoded.Items.Ref.Hash, work.state.Previous.Items.Ref.Hash, work.state.Scalar.Text.Tree.Ref.Hash} {
		heap.allocation(int64(len(hash)), 1)
	}
	for _, values := range []legacyNativeCheckpointStringValues{work.decoded, work.previous} {
		heap.include(legacyNativeStringsBacking(cap(values.Values)))
		heap.allocation(int64(cap(values.Capacities)), int64(unsafe.Sizeof(int(0))))
		heap.include(values.Payload)
	}
	if work.list != nil {
		heap.allocation(1, int64(unsafe.Sizeof(*work.list)))
		if work.list.scalar != nil {
			work.list.scalar.retained(&heap)
		}
	}
	retained, err := work.values.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	retained, err = work.scalar.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointStringListRestore) admit(scratch int64) error {
	retained, err := work.retainedBytes()
	return legacyNativeCheckpointTypedNumberAdmit(retained, err, work.budget, scratch)
}

func (work *legacyNativeCheckpointStringListRestore) take(ctx context.Context) (*legacyNativeStringList, error) {
	if work == nil {
		return nil, ErrIntegrity
	}
	if work.failure != nil {
		return nil, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return nil, work.failure
	}
	if work.closed || !work.ready || work.list == nil {
		work.fail(ErrIntegrity)
		return nil, work.failure
	}
	list := work.list
	work.close()
	return list, nil
}

func (work *legacyNativeCheckpointStringListRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointStringListRestore) close() {
	if work != nil {
		work.values.close()
		work.scalar.close()
		failure := work.failure
		*work = legacyNativeCheckpointStringListRestore{closed: true, failure: failure}
	}
}
