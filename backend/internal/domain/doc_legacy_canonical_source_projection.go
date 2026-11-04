package domain

import (
	"context"
	"unsafe"
)

type legacyCanonicalProjectionSlot string

const (
	legacyCanonicalProjectionRoot        legacyCanonicalProjectionSlot = "root"
	legacyCanonicalProjectionMetadata    legacyCanonicalProjectionSlot = "metadata"
	legacyCanonicalProjectionLocked      legacyCanonicalProjectionSlot = "locked"
	legacyCanonicalProjectionEmptyInput  legacyCanonicalProjectionSlot = "empty-input"
	legacyCanonicalProjectionBlocks      legacyCanonicalProjectionSlot = "blocks"
	legacyCanonicalProjectionReplacement legacyCanonicalProjectionSlot = "replacement"
)

type legacyCanonicalProjectionReference struct {
	Source legacyCanonicalSourceRecordReference `json:"source"`
	Slot   legacyCanonicalProjectionSlot        `json:"slot"`
}

func (reference legacyCanonicalProjectionReference) validate() error {
	if err := reference.Source.validate(); err != nil {
		return err
	}
	switch reference.Slot {
	case legacyCanonicalProjectionRoot, legacyCanonicalProjectionMetadata, legacyCanonicalProjectionLocked,
		legacyCanonicalProjectionEmptyInput, legacyCanonicalProjectionBlocks, legacyCanonicalProjectionReplacement:
		return nil
	default:
		return ErrIntegrity
	}
}

type legacyCanonicalSourceProjection struct {
	reference legacyCanonicalSourceRecordReference
	record    legacyNativeEventValue
	root      map[string]any
	heap      docRangeHeaderHeapEstimate
	closed    bool
}

func legacyCanonicalSourceProjectionMetadata(reference legacyCanonicalSourceRecordReference) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(legacyCanonicalSourceProjection{})))
	heap.allocation(int64(len(reference.Fields.Hash)), 1)
	return heap
}

func (work *legacyCanonicalInitialRestore) projectSourceRecord(ctx context.Context, reference legacyCanonicalSourceRecordReference) (*legacyCanonicalSourceProjection, bool, error) {
	record, found, err := work.resolveSourceRecord(ctx, reference)
	if err != nil || !found {
		return nil, found, err
	}
	if reference.Record == 1 && work.projection != nil {
		if work.projection.closed || work.projection.reference != reference {
			return nil, false, ErrIntegrity
		}
		return work.projection, true, nil
	}
	planned := legacyCanonicalSourceProjectionMetadata(reference)
	planned.include(docRangeHeaderHeapEstimate{bytes: legacyCanonicalReplacementProjectionBytes})
	if planned.err != nil {
		return nil, false, planned.err
	}
	if err := work.admit(planned.bytes); err != nil {
		return nil, false, err
	}
	root, err := legacyCanonicalNativeEventViewJSONObject(record.event, record.replacement, record.blocks)
	if err != nil {
		return nil, false, err
	}
	heap := legacyCanonicalNativeProjectionHeap(record.event, root)
	if heap.err != nil || heap.bytes > legacyCanonicalReplacementProjectionBytes {
		return nil, false, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	projection := &legacyCanonicalSourceProjection{reference: reference, record: record, root: root, heap: heap}
	if reference.Record == 1 {
		work.projection = projection
	}
	return projection, true, nil
}

func (projection *legacyCanonicalSourceProjection) retainedBytes() (int64, error) {
	if projection == nil || projection.closed {
		return 0, nil
	}
	heap := legacyCanonicalSourceProjectionMetadata(projection.reference)
	heap.include(projection.heap)
	return heap.bytes, heap.err
}

func (projection *legacyCanonicalSourceProjection) resolve(ctx context.Context, reference legacyCanonicalProjectionReference) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := reference.validate(); err != nil {
		return nil, err
	}
	if projection == nil || projection.closed || projection.root == nil || projection.reference != reference.Source {
		return nil, ErrIntegrity
	}
	if reference.Slot == legacyCanonicalProjectionRoot {
		return projection.root, nil
	}
	key := ""
	switch reference.Slot {
	case legacyCanonicalProjectionMetadata:
		key = "provider_metadata"
	case legacyCanonicalProjectionLocked:
		key = "locked"
	case legacyCanonicalProjectionEmptyInput:
		if projection.record.event.Kind != EventToolCall || projection.record.event.Input != nil {
			return nil, ErrIntegrity
		}
		key = "input"
	case legacyCanonicalProjectionBlocks:
		key = "blocks"
	case legacyCanonicalProjectionReplacement:
		key = "replacement"
	}
	value, exists := projection.root[key]
	if !exists || value == nil {
		return nil, ErrIntegrity
	}
	return value, nil
}

func (projection *legacyCanonicalSourceProjection) takeEncoding(ctx context.Context) (*LegacyCanonicalEncoding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if projection == nil || projection.closed || projection.reference.Record != 1 || projection.reference.validate() != nil || projection.root == nil {
		return nil, ErrIntegrity
	}
	owner := &legacyNativeCanonicalOwner{event: projection.record.event, replacement: projection.record.replacement,
		blocks: projection.record.blocks, heap: projection.record.heap}
	owner.seal = owner
	_, heap, err := owner.claim(ctx)
	if err != nil {
		owner.release()
		return nil, err
	}
	heap.include(projection.heap)
	root := projection.root
	projection.close()
	if heap.err != nil {
		owner.release()
		return nil, heap.err
	}
	return finishLegacyCanonicalEncodingNative(ctx, owner, root, heap)
}

func (projection *legacyCanonicalSourceProjection) close() {
	if projection != nil {
		*projection = legacyCanonicalSourceProjection{closed: true}
	}
}
