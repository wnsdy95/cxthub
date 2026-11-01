package domain

import (
	"context"
	"unsafe"
)

type legacyCanonicalPreparedFieldState struct {
	Key       legacyNativeCheckpointRef `json:"key"`
	Value     legacyNativeCheckpointRef `json:"value"`
	ChildKeys int                       `json:"child_keys"`
}

type legacyCanonicalPreparedFieldsWrite struct {
	source   *legacyCanonicalPages[legacyCanonicalKeyField]
	numbers  *legacyCanonicalNumberValidation
	binding  ContentHash
	store    legacyNativeCheckpointIO
	budget   int64
	position int
	valueAt  int
	keyAt    int
	keySkip  int
	phase    uint8
	value    *legacyCanonicalPreparedValueWrite
	record   legacyCanonicalPreparedFieldState
	list     legacyNativeCheckpointListWriter[legacyCanonicalPreparedFieldState]
	tree     legacyNativeCheckpointTree
	ready    bool
}

func (work *legacyCanonicalPreparedFieldsWrite) retainedBytes() (int64, error) {
	if work == nil {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	retained, err := work.list.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	if work.value != nil {
		retained, err = work.value.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyCanonicalPreparedFieldsWrite) step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if work == nil || work.source == nil || !work.source.valid() || work.position < 0 || work.position > work.source.length() {
		return false, ErrIntegrity
	}
	if work.ready {
		return true, nil
	}
	retained, err := work.retainedBytes()
	if err != nil || retained+legacyNativeAnyCheckpointScratch > work.budget {
		return false, ErrDocumentResourceLimit
	}
	if work.position == work.source.length() {
		work.tree, err = work.list.finish(ctx, work.store)
		work.ready = err == nil
		return work.ready, err
	}
	field := work.source.at(work.position)
	if field == nil {
		return false, ErrIntegrity
	}
	if work.phase == 0 || work.phase == 2 {
		var value any
		if work.phase == 0 {
			if work.position >= work.keyAt && work.position != work.keySkip {
				value = field.key
			} else {
				value = ""
			}
		} else if work.position >= work.valueAt {
			value = field.value
		}
		work.value, err = newLegacyCanonicalPreparedValueWrite(ctx, value, work.numbers, work.binding, work.store, work.budget-retained)
		if err != nil {
			return false, err
		}
		work.phase++
		retained, err = work.retainedBytes()
		if err != nil || retained+legacyNativeAnyCheckpointScratch > work.budget {
			return false, ErrDocumentResourceLimit
		}
	}
	if work.phase == 1 || work.phase == 3 {
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
		if work.phase == 1 {
			work.record.Key = ref
		} else {
			work.record.Value = ref
		}
		work.phase++
		return false, nil
	}
	work.record.ChildKeys = field.childKeys
	if err := work.list.append(ctx, work.store, work.record); err != nil {
		return false, err
	}
	work.position++
	work.record, work.phase = legacyCanonicalPreparedFieldState{}, 0
	return false, nil
}

func (work *legacyCanonicalPreparedFieldsWrite) close() {
	if work != nil {
		if work.value != nil {
			work.value.close()
		}
		work.list.close()
		*work = legacyCanonicalPreparedFieldsWrite{}
	}
}

type legacyCanonicalPreparedFieldsRestore struct {
	reader  *legacyCanonicalPreparedListReader[legacyCanonicalPreparedFieldState]
	numbers *legacyCanonicalNumberValidation
	binding ContentHash
	store   legacyNativeCheckpointIO
	budget  int64
	fields  legacyCanonicalPages[legacyCanonicalKeyField]
	payload docRangeHeaderHeapEstimate
	record  legacyCanonicalPreparedFieldState
	field   legacyCanonicalKeyField
	phase   uint8
	value   *legacyCanonicalPreparedValueRestore
	ready   bool
}

func (work *legacyCanonicalPreparedFieldsRestore) retainedBytes() (int64, error) {
	var heap docRangeHeaderHeapEstimate
	if work == nil {
		return 0, nil
	}
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(work.payload)
	work.fields.retained(&heap)
	if work.reader != nil {
		retained, err := work.reader.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.value != nil {
		retained, err := work.value.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyCanonicalPreparedFieldsRestore) step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if work == nil || work.reader == nil {
		return false, ErrIntegrity
	}
	if work.ready {
		return true, nil
	}
	retained, err := work.retainedBytes()
	if err != nil || retained+legacyNativeAnyCheckpointScratch > work.budget {
		return false, ErrDocumentResourceLimit
	}
	if work.phase == 0 {
		record, found, err := work.reader.next(ctx)
		if err != nil || !found {
			work.ready = err == nil
			return work.ready, err
		}
		if !record.Key.valid() || !record.Value.valid() || record.ChildKeys < 0 {
			return false, ErrIntegrity
		}
		work.record, work.phase = record, 1
		return false, nil
	}
	if work.phase == 1 || work.phase == 3 {
		ref := work.record.Key
		if work.phase == 3 {
			ref = work.record.Value
		}
		work.value, err = newLegacyCanonicalPreparedValueRestore(ctx, ref, work.numbers, work.binding, work.store, work.budget-retained)
		if err == nil {
			work.phase++
		}
		return false, err
	}
	if work.phase == 2 || work.phase == 4 {
		ready, err := work.value.step(ctx)
		if err != nil || !ready {
			return false, err
		}
		value, payload, err := work.value.take(ctx)
		work.value = nil
		if err != nil {
			return false, err
		}
		work.payload.include(payload)
		if work.phase == 2 {
			key, valid := value.(string)
			if !valid {
				return false, ErrIntegrity
			}
			work.field.key = key
		} else {
			work.field.value = value
		}
		work.phase++
		return false, work.payload.err
	}
	work.field.childKeys = work.record.ChildKeys
	_, forecast, err := work.fields.appendRequirements()
	if err != nil {
		return false, err
	}
	var planned docRangeHeaderHeapEstimate
	planned.include(work.payload)
	planned.include(forecast)
	planned.allocation(1, int64(unsafe.Sizeof(*work)))
	if planned.err != nil || planned.bytes+legacyNativeAnyCheckpointScratch > work.budget {
		return false, ErrDocumentResourceLimit
	}
	if err := work.fields.append(work.field); err != nil {
		return false, err
	}
	work.record, work.field, work.phase = legacyCanonicalPreparedFieldState{}, legacyCanonicalKeyField{}, 0
	return false, nil
}

func (work *legacyCanonicalPreparedFieldsRestore) close() {
	if work != nil {
		if work.value != nil {
			work.value.close()
		}
		if work.reader != nil {
			work.reader.close()
		}
		*work = legacyCanonicalPreparedFieldsRestore{}
	}
}
