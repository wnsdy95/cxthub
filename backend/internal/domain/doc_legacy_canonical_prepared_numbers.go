package domain

import (
	"context"
	"unsafe"
)

type legacyCanonicalPreparedNumberState struct {
	Pending  legacyNativeCheckpointTree `json:"pending"`
	Next     int                        `json:"next"`
	Position int                        `json:"position"`
	Mode     legacyJSONValueMode        `json:"mode"`
}

type legacyCanonicalPreparedNumberRow struct {
	Version  uint8                      `json:"v"`
	Binding  ContentHash                `json:"binding,omitempty"`
	Ordinal  int                        `json:"ordinal"`
	Text     legacyNativeCheckpointText `json:"text"`
	Proven   bool                       `json:"proven"`
	Next     int                        `json:"next"`
	Position int                        `json:"position"`
	Mode     legacyJSONValueMode        `json:"mode"`
}

func legacyCanonicalPreparedNumberCursor(next, position int, mode legacyJSONValueMode, count int) error {
	if count < 0 || uint64(count) >= legacyNativeValueCheckpointPayloadLimit() || next < 0 || next > count ||
		position < 0 || uint64(position) > legacyNativeValueCheckpointPayloadLimit() ||
		next == count && position != 0 || position == 0 && mode != legacyJSONValueIdle ||
		position > 0 && (mode < legacyJSONValueMinus || mode > legacyJSONValueExponentDigits) {
		return ErrIntegrity
	}
	return nil
}

func (state legacyCanonicalPreparedNumberState) header(binding ContentHash) legacyCanonicalPreparedNumberRow {
	return legacyCanonicalPreparedNumberRow{
		Version: legacyNativeCheckpointVersion, Binding: binding, Ordinal: -1,
		Next: state.Next, Position: state.Position, Mode: state.Mode,
	}
}

func legacyCanonicalPreparedNumbersWorkspace() docRangeHeaderHeapEstimate {
	heap := legacyNativeCheckpointStringValuesWorkspace()
	heap.allocation(legacyNativeCheckpointPageItems, int64(unsafe.Sizeof(legacyCanonicalPreparedNumberRow{})))
	return heap
}

type legacyCanonicalPreparedNumbersCheckpoint struct {
	numbers   *legacyCanonicalNumberValidation
	state     legacyCanonicalPreparedNumberState
	binding   ContentHash
	store     legacyNativeCheckpointIO
	rows      *legacyNativeCheckpointListWriter[legacyCanonicalPreparedNumberRow]
	text      *legacyNativeCheckpointTextWriter
	row       legacyCanonicalPreparedNumberRow
	texts     legacyCanonicalSourceNumberTexts
	keepTexts bool
	count     int
	position  int
	budget    int64
	header    bool
	pending   bool
	ready     bool
	closed    bool
	failure   error
}

func newLegacyCanonicalPreparedNumbersCheckpoint(ctx context.Context, numbers *legacyCanonicalNumberValidation, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalPreparedNumbersCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if numbers == nil || !numbers.pending.valid() || !numbers.proven.valid() ||
		numbers.pending.length() != numbers.proven.length() || store.Put == nil || budget <= 0 ||
		len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil {
		return nil, ErrIntegrity
	}
	count := numbers.pending.length()
	if err := legacyCanonicalPreparedNumberCursor(numbers.next, numbers.position, numbers.mode, count); err != nil {
		return nil, err
	}
	if numbers.next < count {
		text := numbers.pending.at(numbers.next)
		if text == nil || numbers.position > len(*text) {
			return nil, ErrIntegrity
		}
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyCanonicalPreparedNumbersCheckpoint{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointListWriter[legacyCanonicalPreparedNumberRow]{})))
	planned.allocation(int64(len(binding)), 1)
	planned.include(legacyCanonicalPreparedNumbersWorkspace())
	if err := legacyNativeCheckpointStringValuesBudget(0, planned, budget); err != nil {
		return nil, err
	}
	return &legacyCanonicalPreparedNumbersCheckpoint{
		numbers: numbers, binding: binding, store: store, count: count, budget: budget,
		state: legacyCanonicalPreparedNumberState{Next: numbers.next, Position: numbers.position, Mode: numbers.mode},
		rows:  &legacyNativeCheckpointListWriter[legacyCanonicalPreparedNumberRow]{},
	}, nil
}

func (work *legacyCanonicalPreparedNumbersCheckpoint) step(ctx context.Context) (bool, error) {
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
	if err := work.checkBudget(legacyCanonicalPreparedNumbersWorkspace()); err != nil {
		return work.fail(err)
	}
	if !work.header {
		if err := work.rows.append(ctx, work.store, work.state.header(work.binding)); err != nil {
			return work.fail(err)
		}
		work.header = true
		return work.progress(ctx)
	}
	if work.text != nil {
		ready, err := work.text.step(ctx)
		if err != nil {
			return work.fail(err)
		}
		if ready {
			work.row.Text, err = work.text.take(ctx)
			if err != nil {
				return work.fail(err)
			}
			work.text, work.pending = nil, true
		}
		return work.progress(ctx)
	}
	if work.pending {
		if work.keepTexts && work.texts.values.length() == work.position {
			if err := work.texts.append(work.row.Text, max(1, work.row.Text.Bytes), work.checkBudget); err != nil {
				return work.fail(err)
			}
			return work.progress(ctx)
		}
		if err := work.rows.append(ctx, work.store, work.row); err != nil {
			return work.fail(err)
		}
		work.position++
		work.row, work.pending = legacyCanonicalPreparedNumberRow{}, false
		return work.progress(ctx)
	}
	if work.position < work.count {
		pending := work.numbers.pending.at(work.position)
		if pending == nil || uint64(len(*pending)) > legacyNativeValueCheckpointPayloadLimit() {
			return work.fail(ErrIntegrity)
		}
		entry, err := work.numbers.proven.lookup(legacyCanonicalNumberKey(*pending))
		if err != nil || entry.ordinal != work.position || entry.proven != (work.position < work.state.Next) {
			return work.fail(ErrIntegrity)
		}
		work.row = legacyCanonicalPreparedNumberRow{
			Version: legacyNativeCheckpointVersion, Ordinal: work.position, Proven: entry.proven,
		}
		work.text, err = newLegacyNativeCheckpointTextWriter(ctx, len(*pending), work.read, work.store)
		if err != nil {
			return work.fail(err)
		}
		return work.progress(ctx)
	}
	var err error
	work.state.Pending, err = work.rows.finish(ctx, work.store)
	if err != nil {
		return work.fail(err)
	}
	if work.state.Pending.Count != uint64(work.count)+1 || (work.keepTexts && work.texts.values.length() != work.count) {
		return work.fail(ErrIntegrity)
	}
	work.rows.close()
	work.rows, work.numbers = nil, nil
	work.ready = true
	return work.progress(ctx)
}

func (work *legacyCanonicalPreparedNumbersCheckpoint) read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if work == nil || work.closed || work.numbers == nil {
		return nil, ErrIntegrity
	}
	pending := work.numbers.pending.at(work.position)
	if pending == nil || offset < 0 || offset > len(*pending) || length <= 0 ||
		length > LegacyCanonicalEncodingBytes || length > len(*pending)-offset {
		return nil, ErrIntegrity
	}
	body := []byte((*pending)[offset : offset+length])
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

func (work *legacyCanonicalPreparedNumbersCheckpoint) progress(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkBudget(docRangeHeaderHeapEstimate{}); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyCanonicalPreparedNumbersCheckpoint) checkBudget(additional docRangeHeaderHeapEstimate) error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	return legacyNativeCheckpointStringValuesBudget(retained, additional, work.budget)
}

func (work *legacyCanonicalPreparedNumbersCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.binding)), 1)
	heap.allocation(int64(len(work.state.Pending.Ref.Hash)), 1)
	heap.allocation(int64(len(work.row.Text.Tree.Ref.Hash)), 1)
	work.texts.retained(&heap)
	if work.rows != nil {
		retained, err := work.rows.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.text != nil {
		retained, err := work.text.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		heap.allocation(1, 64)
	}
	return heap.bytes, heap.err
}

func (work *legacyCanonicalPreparedNumbersCheckpoint) take(ctx context.Context) (legacyCanonicalPreparedNumberState, error) {
	if work == nil {
		return legacyCanonicalPreparedNumberState{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyCanonicalPreparedNumberState{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyCanonicalPreparedNumberState{}, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyCanonicalPreparedNumberState{}, ErrIntegrity
	}
	state := work.state
	work.close()
	return state, nil
}

func (work *legacyCanonicalPreparedNumbersCheckpoint) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyCanonicalPreparedNumbersCheckpoint) close() {
	if work != nil {
		if work.rows != nil {
			work.rows.close()
		}
		work.text.close()
		failure := work.failure
		*work = legacyCanonicalPreparedNumbersCheckpoint{closed: true, failure: failure}
	}
}
