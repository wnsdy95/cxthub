package domain

import (
	"context"
	"errors"
	"strings"
	"unsafe"
)

type legacyNativeScalarCheckpoint struct {
	Version  uint8 `json:"version"`
	Position int   `json:"position"`
	End      int   `json:"end"`
	Maximum  int   `json:"maximum"`
	Bytes    int   `json:"bytes"`
	Capacity int   `json:"capacity"`
	Present  bool  `json:"present"`
	Expanded bool  `json:"expanded"`
}

func legacyNativeScalarAllocationBound(capacity int) int64 {
	const pageBytes = int64(8 << 10)
	return (int64(capacity) + pageBytes - 1) / pageBytes * pageBytes
}

func (state legacyNativeScalarCheckpoint) validate(sourceBytes int) error {
	maximum := int64(MaxFinalizedDocBytes)*6 + int64(len(legacyNativeTypedNumberErrorPrefix)+len(legacyNativeTypedNumberErrorSuffix)+2)
	if state.Version != 1 || sourceBytes < 0 || sourceBytes > MaxFinalizedDocBytes ||
		state.Position < 0 || state.Position > state.End || state.End > sourceBytes ||
		state.Maximum < 0 || int64(state.Maximum) > maximum || state.Bytes < 0 || state.Bytes > state.Maximum ||
		state.Capacity < state.Bytes || int64(state.Capacity) > 2*int64(state.Maximum)+64 ||
		!state.Present && (state.Bytes != 0 || state.Capacity != 0) {
		return ErrIntegrity
	}
	return nil
}

type legacyNativeScalarSnapshot struct {
	state    legacyNativeScalarCheckpoint
	prefix   string
	capacity int
	closed   bool
}

func (scalar *legacyNativeScalar) checkpoint(ctx context.Context) (*legacyNativeScalarSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scalar == nil {
		return nil, ErrIntegrity
	}
	state := legacyNativeScalarCheckpoint{
		Version: 1, Position: scalar.position, End: scalar.end, Maximum: scalar.maximum,
		Present: scalar.text != nil, Expanded: scalar.expanded,
	}
	accepted := scalar.text
	if scalar.text != nil {
		state.Capacity = scalar.text.Cap()
	}
	if scalar.previous != nil {
		if scalar.text == nil || scalar.previous == scalar.text || scalar.previous.Len() == 0 ||
			scalar.copied < 0 || scalar.copied >= scalar.previous.Len() || scalar.text.Len() != scalar.copied {
			return nil, ErrIntegrity
		}
		accepted = scalar.previous
	}
	if accepted != nil {
		state.Bytes = accepted.Len()
	}
	if err := state.validate(MaxFinalizedDocBytes); err != nil {
		return nil, err
	}
	if scalar.buffered < 0 || scalar.buffered > len(scalar.pending) || scalar.buffered > scalar.end-scalar.position {
		return nil, ErrIntegrity
	}
	snapshot := &legacyNativeScalarSnapshot{state: state}
	if accepted != nil {
		snapshot.prefix, snapshot.capacity = accepted.String(), accepted.Cap()
	}
	return snapshot, nil
}

func (snapshot *legacyNativeScalarSnapshot) read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.closed || offset < 0 || offset > len(snapshot.prefix) ||
		length < 0 || length > LegacyCanonicalEncodingBytes || length > len(snapshot.prefix)-offset {
		return nil, ErrIntegrity
	}
	body := []byte(snapshot.prefix[offset : offset+length])
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

func (snapshot *legacyNativeScalarSnapshot) retainedBytes() (int64, error) {
	if snapshot == nil || snapshot.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*snapshot)))
	heap.allocation(int64(snapshot.capacity), 1)
	return heap.bytes, heap.err
}

func (snapshot *legacyNativeScalarSnapshot) close() {
	if snapshot != nil {
		*snapshot = legacyNativeScalarSnapshot{closed: true}
	}
}

type legacyNativeScalarRestore struct {
	state      legacyNativeScalarCheckpoint
	source     LegacyByteSource
	sourceHeap docRangeHeaderHeapEstimate
	scalar     *legacyNativeScalar
	page       []byte
	copied     int
	budget     int64
	ready      bool
	closed     bool
	failure    error
}

func newLegacyNativeScalarRestore(ctx context.Context, state legacyNativeScalarCheckpoint, prefix LegacyByteSource, sourceBytes int, heapBudget int64) (*legacyNativeScalarRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := state.validate(sourceBytes); err != nil {
		return nil, err
	}
	partBytes := prefix.partBytes()
	if heapBudget <= 0 || partBytes == 0 || prefix.Bytes != state.Bytes || prefix.Bytes > 0 && prefix.Read == nil {
		return nil, ErrIntegrity
	}
	parts := 0
	if prefix.Bytes > 0 {
		parts = 1 + (prefix.Bytes-1)/partBytes
	}
	if len(prefix.Parts) != parts {
		return nil, ErrIntegrity
	}
	var sourceHeap docRangeHeaderHeapEstimate
	sourceHeap.allocation(int64(len(prefix.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	for ordinal, part := range prefix.Parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if ValidateContentHash(part.Hash) != nil || part.Bytes != int64(min(partBytes, prefix.Bytes-ordinal*partBytes)) {
			return nil, ErrIntegrity
		}
		sourceHeap.allocation(int64(len(part.Hash)), 1)
	}
	var planned docRangeHeaderHeapEstimate
	planned.include(sourceHeap)
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeScalarRestore{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeScalar{})))
	if state.Present {
		planned.allocation(1, int64(unsafe.Sizeof(strings.Builder{})))
		planned.allocation(legacyNativeScalarAllocationBound(state.Capacity), 1)
	}
	planned.allocation(legacyNativeScalarAllocationBound(min(partBytes, prefix.Bytes)), 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > heapBudget {
		return nil, ErrDocumentResourceLimit
	}
	partsCopy := make([]ConversationManifestChunk, len(prefix.Parts))
	copy(partsCopy, prefix.Parts)
	prefix.Parts = partsCopy
	for ordinal := range prefix.Parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		prefix.Parts[ordinal].Hash = ContentHash(strings.Clone(string(prefix.Parts[ordinal].Hash)))
	}
	if prefix.Bytes == 0 {
		prefix.Read = nil
	}
	return &legacyNativeScalarRestore{state: state, source: prefix, sourceHeap: sourceHeap, budget: heapBudget}, nil
}

func (work *legacyNativeScalarRestore) step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		if work != nil && work.failure == nil {
			work.fail(err)
		}
		return false, err
	}
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if work.closed {
		return false, ErrIntegrity
	}
	if work.state.Version != 1 || work.budget <= 0 {
		return work.fail(ErrIntegrity)
	}
	if work.ready {
		return true, nil
	}
	if work.scalar == nil {
		work.scalar = &legacyNativeScalar{
			position: work.state.Position, end: work.state.End,
			maximum: work.state.Maximum, expanded: work.state.Expanded,
		}
		if work.state.Present {
			work.scalar.text = &strings.Builder{}
			work.scalar.text.Grow(work.state.Capacity)
		}
		if err := work.checkBudget(ctx); err != nil {
			return work.fail(err)
		}
		return false, nil
	}
	if work.copied < work.state.Bytes {
		partBytes := work.source.partBytes()
		if work.page == nil {
			page, err := readLegacyByteSourcePart(ctx, work.source, work.copied/partBytes)
			if err != nil {
				if ctx.Err() != nil {
					return work.fail(ctx.Err())
				}
				return work.fail(errors.Join(ErrPreparedDocRangeObject, err))
			}
			work.page = page
		}
		if err := work.checkBudget(ctx); err != nil {
			return work.fail(err)
		}
		offset := work.copied % partBytes
		length := min(LegacyCanonicalEncodingBytes, len(work.page)-offset, work.state.Bytes-work.copied)
		if length <= 0 || work.scalar.text == nil || work.scalar.text.Cap()-work.scalar.text.Len() < length {
			return work.fail(ErrIntegrity)
		}
		_, _ = work.scalar.text.Write(work.page[offset : offset+length])
		work.copied += length
		if offset+length == len(work.page) {
			work.page = nil
		}
		if err := ctx.Err(); err != nil {
			return work.fail(err)
		}
	}
	if work.copied == work.state.Bytes {
		work.page = nil
		work.source = LegacyByteSource{}
		work.sourceHeap = docRangeHeaderHeapEstimate{}
		work.ready = true
	}
	return work.ready, nil
}

func (work *legacyNativeScalarRestore) checkBudget(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	heap, err := work.retainedBytes()
	if err != nil {
		return err
	}
	if heap > work.budget {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeScalarRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(work.sourceHeap)
	heap.allocation(int64(cap(work.page)), 1)
	if work.scalar != nil {
		work.scalar.retained(&heap)
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeScalarRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeScalarRestore) take(ctx context.Context) (*legacyNativeScalar, error) {
	if err := ctx.Err(); err != nil {
		if work != nil && work.failure == nil {
			work.fail(err)
		}
		return nil, err
	}
	if work == nil {
		return nil, ErrIntegrity
	}
	if work.failure != nil {
		return nil, work.failure
	}
	if work.closed {
		return nil, ErrIntegrity
	}
	if !work.ready || work.scalar == nil {
		_, err := work.fail(ErrIntegrity)
		return nil, err
	}
	scalar := work.scalar
	work.close()
	return scalar, nil
}

func (work *legacyNativeScalarRestore) close() {
	if work != nil {
		failure := work.failure
		*work = legacyNativeScalarRestore{closed: true, failure: failure}
	}
}
