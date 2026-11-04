package domain

import (
	"context"
	"encoding/json"
	"strings"
	"unsafe"
)

const legacyNativeBlockInlineCapacity = 256
const legacyNativeBlockInlineEncodedBytes = 1024

func legacyNativeBlockInlineRecord(ctx context.Context, record legacyNativeBlockValue, initialized bool) (legacyNativeCheckpointBlockRecord, bool, error) {
	var empty legacyNativeCheckpointBlockRecord
	if err := ctx.Err(); err != nil {
		return empty, false, err
	}
	schema, err := legacyNativeCheckpointBlockSchema()
	if err != nil {
		return empty, false, err
	}
	capacities, err := legacyNativeBlockRecordCapacities(record, initialized)
	if err != nil {
		return empty, false, err
	}
	if capacities[0] > legacyNativeBlockInlineCapacity || capacities[1] > legacyNativeBlockInlineCapacity-capacities[0] {
		return empty, false, nil
	}
	inline := &legacyNativeCheckpointBlockInlineRecord{Capacities: capacities}
	for ordinal, text := range [2]string{record.block.Type, record.block.Text} {
		inline.Values[ordinal] = []byte(text)
	}
	result := legacyNativeCheckpointBlockRecord{Schema: schema, Inline: inline}
	body, err := json.Marshal(result)
	if err != nil || len(body) > legacyNativeBlockInlineEncodedBytes {
		return empty, false, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return empty, false, err
	}
	return result, true, nil
}

func legacyNativeBlockInlinePlan(saved legacyNativeCheckpointBlockRecord) (docRangeHeaderHeapEstimate, error) {
	var empty docRangeHeaderHeapEstimate
	schema, err := legacyNativeCheckpointBlockSchema()
	if err != nil || saved.Schema != schema || saved.Inline == nil || saved.Values != (legacyNativeCheckpointStrings{}) {
		return empty, ErrIntegrity
	}
	inline := saved.Inline
	var planned docRangeHeaderHeapEstimate
	capacity := 0
	for ordinal, body := range inline.Values {
		current := inline.Capacities[ordinal]
		if body == nil || current < len(body) || current > legacyNativeBlockInlineCapacity-capacity {
			return empty, ErrIntegrity
		}
		capacity += current
		planned.allocation(legacyNativeScalarAllocationBound(current), 1)
	}
	body, err := json.Marshal(saved)
	if err != nil || len(body) > legacyNativeBlockInlineEncodedBytes {
		return empty, ErrIntegrity
	}
	planned.allocation(2, int64(unsafe.Sizeof(strings.Builder{})))
	return planned, planned.err
}

func legacyNativeBlockInlineRestore(ctx context.Context, saved legacyNativeCheckpointBlockRecord, sourceBytes int, budget int64) (legacyNativeBlockValue, error) {
	var empty legacyNativeBlockValue
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if sourceBytes < 0 || sourceBytes > MaxFinalizedDocBytes || budget <= 0 {
		return empty, ErrIntegrity
	}
	planned, err := legacyNativeBlockInlinePlan(saved)
	if err != nil {
		return empty, err
	}
	if planned.bytes > budget {
		return empty, ErrDocumentResourceLimit
	}
	var values [2]string
	result := legacyNativeBlockValue{}
	inline := saved.Inline
	for ordinal, body := range inline.Values {
		capacity := inline.Capacities[ordinal]
		if capacity == len(body) {
			owned := make([]byte, capacity)
			copy(owned, body)
			values[ordinal] = unsafe.String(unsafe.SliceData(owned), len(owned))
		} else {
			var builder strings.Builder
			builder.Grow(capacity)
			if builder.Cap() != capacity {
				return empty, ErrIntegrity
			}
			builder.Write(body)
			values[ordinal] = builder.String()
		}
		result.heaps[ordinal].allocation(int64(capacity), 1)
		result.excess[ordinal] = 2 * int64(capacity-len(body))
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	result.block = ContentBlock{Type: values[0], Text: values[1]}
	return result, nil
}
