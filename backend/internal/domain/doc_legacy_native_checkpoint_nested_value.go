package domain

import (
	"context"
	"encoding/json"
	"reflect"
	"unsafe"
)

func legacyNativeCheckpointNestedValueSchema(model uint8) (ContentHash, int, error) {
	var expected reflect.Type
	var count int
	switch model {
	case legacyNativeCheckpointLockedModel:
		expected, count = reflect.TypeFor[LockedBlob](), 3
	case legacyNativeCheckpointProviderModel:
		expected, count = reflect.TypeFor[ProviderMetadata](), 2
	default:
		return "", 0, ErrIntegrity
	}
	metadata := legacyNativeCheckpointModelMetadata(model)
	if metadata == nil || metadata.err != nil || metadata.model != expected ||
		expected.NumField() != count || len(metadata.fields) != count {
		return "", 0, ErrIntegrity
	}
	for ordinal, field := range metadata.fields {
		if field.index != ordinal {
			return "", 0, ErrIntegrity
		}
		if model == legacyNativeCheckpointProviderModel && ordinal == 1 {
			if !field.typedNumber || expected.Field(ordinal).Type != reflect.TypeFor[*json.Number]() {
				return "", 0, ErrIntegrity
			}
		} else if !field.scalarString || expected.Field(ordinal).Type.Kind() != reflect.String {
			return "", 0, ErrIntegrity
		}
	}
	schema, err := legacyNativeCheckpointModelSchema(metadata)
	if err != nil {
		return "", 0, err
	}
	return schema, count, nil
}

func (saved legacyNativeCheckpointNestedValue) validate(expectedModel uint8) error {
	schema, count, err := legacyNativeCheckpointNestedValueSchema(expectedModel)
	if err != nil || saved.Version != legacyNativeCheckpointVersion || saved.Model != expectedModel ||
		saved.Schema != schema || expectedModel == legacyNativeCheckpointLockedModel && saved.NumberPresent ||
		saved.Values.Nil || saved.Values.Length != count || saved.Values.Capacity != count {
		return ErrIntegrity
	}
	return saved.Values.validate(true)
}

func legacyNativeCheckpointNestedValueStorage(model uint8, numberPresent bool) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof((*LockedBlob)(nil))))
	switch model {
	case legacyNativeCheckpointLockedModel:
		if numberPresent {
			heap.err = ErrIntegrity
			return heap
		}
		heap.allocation(1, int64(unsafe.Sizeof(LockedBlob{})))
	case legacyNativeCheckpointProviderModel:
		heap.allocation(1, int64(unsafe.Sizeof(ProviderMetadata{})))
		if numberPresent {
			heap.allocation(1, int64(unsafe.Sizeof(json.Number(""))))
		}
	default:
		heap.err = ErrIntegrity
	}
	return heap
}

type legacyNativeCheckpointNestedValueWriter struct {
	values  []string
	strings *legacyNativeCheckpointStringValuesWriter
	result  legacyNativeCheckpointNestedValue
	budget  int64
	ready   bool
	closed  bool
	failure error
}

func newLegacyNativeCheckpointNestedValueWriter(ctx context.Context, value reflect.Value, model uint8, store legacyNativeCheckpointIO, budget int64) (*legacyNativeCheckpointNestedValueWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	schema, count, err := legacyNativeCheckpointNestedValueSchema(model)
	if err != nil {
		return nil, err
	}
	metadata := legacyNativeCheckpointModelMetadata(model)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() ||
		value.Type() != reflect.PointerTo(metadata.model) || store.Put == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	var texts [3]string
	var numberPresent bool
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointNestedValueWriter{})))
	planned.allocation(int64(count), int64(unsafe.Sizeof(string(""))))
	planned.allocation(int64(len(schema)), 1)
	for ordinal := 0; ordinal < count; ordinal++ {
		member := value.Elem().Field(ordinal)
		if model == legacyNativeCheckpointProviderModel && ordinal == 1 {
			if !member.IsNil() {
				numberPresent = true
				texts[ordinal] = member.Elem().String()
			}
		} else {
			texts[ordinal] = member.String()
		}
		if uint64(len(texts[ordinal])) > legacyNativeValueCheckpointPayloadLimit() {
			return nil, ErrIntegrity
		}
		planned.allocation(int64(len(texts[ordinal])), 1)
	}
	if err := legacyNativeCheckpointStringValuesBudget(0, planned, budget); err != nil {
		return nil, err
	}
	if planned.bytes == budget {
		return nil, ErrDocumentResourceLimit
	}
	work := &legacyNativeCheckpointNestedValueWriter{
		values: make([]string, count), budget: budget,
		result: legacyNativeCheckpointNestedValue{
			Version: legacyNativeCheckpointVersion, Model: model, Schema: schema, NumberPresent: numberPresent,
		},
	}
	copy(work.values, texts[:count])
	work.strings, err = newLegacyNativeCheckpointStringValuesWriter(ctx, work.values, nil, store, budget-planned.bytes)
	if err != nil {
		work.close()
		return nil, err
	}
	if _, err := work.progress(ctx); err != nil {
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeCheckpointNestedValueWriter) step(ctx context.Context) (bool, error) {
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
		return work.progress(ctx)
	}
	if work.strings == nil {
		return work.fail(ErrIntegrity)
	}
	ready, err := work.strings.step(ctx)
	if err != nil {
		return work.fail(err)
	}
	if ready {
		work.result.Values, err = work.strings.take(ctx)
		if err != nil {
			return work.fail(err)
		}
		work.strings, work.values = nil, nil
		if err := work.result.validate(work.result.Model); err != nil {
			return work.fail(err)
		}
		work.ready = true
	}
	return work.progress(ctx)
}

func (work *legacyNativeCheckpointNestedValueWriter) progress(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	retained, err := work.retainedBytes()
	if err := legacyNativeCheckpointStringValuesBudget(retained, docRangeHeaderHeapEstimate{err: err}, work.budget); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointNestedValueWriter) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(cap(work.values)), int64(unsafe.Sizeof(string(""))))
	for _, text := range work.values {
		heap.allocation(int64(len(text)), 1)
	}
	heap.allocation(int64(len(work.result.Schema)), 1)
	heap.allocation(int64(len(work.result.Values.Items.Ref.Hash)), 1)
	retained, err := work.strings.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointNestedValueWriter) take(ctx context.Context) (legacyNativeCheckpointNestedValue, error) {
	if work == nil {
		return legacyNativeCheckpointNestedValue{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointNestedValue{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointNestedValue{}, work.failure
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointNestedValue{}, work.failure
	}
	if _, err := work.progress(ctx); err != nil {
		return legacyNativeCheckpointNestedValue{}, err
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeCheckpointNestedValueWriter) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointNestedValueWriter) close() {
	if work != nil {
		work.strings.close()
		failure := work.failure
		*work = legacyNativeCheckpointNestedValueWriter{closed: true, failure: failure}
	}
}

type legacyNativeCheckpointNestedValueRestore struct {
	strings       *legacyNativeCheckpointStringValuesRestore
	values        legacyNativeCheckpointStringValues
	result        reflect.Value
	heap          docRangeHeaderHeapEstimate
	model         uint8
	numberPresent bool
	budget        int64
	ready         bool
	closed        bool
	failure       error
}

func newLegacyNativeCheckpointNestedValueRestore(ctx context.Context, saved legacyNativeCheckpointNestedValue, expectedModel uint8, store legacyNativeCheckpointIO, sourceBytes int, budget int64) (*legacyNativeCheckpointNestedValueRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := saved.validate(expectedModel); err != nil {
		return nil, err
	}
	if store.Read == nil || sourceBytes < 0 || sourceBytes > MaxFinalizedDocBytes || budget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointNestedValueRestore{})))
	planned.include(legacyNativeCheckpointNestedValueStorage(expectedModel, saved.NumberPresent))
	if err := legacyNativeCheckpointStringValuesBudget(0, planned, budget); err != nil {
		return nil, err
	}
	if planned.bytes == budget {
		return nil, ErrDocumentResourceLimit
	}
	work := &legacyNativeCheckpointNestedValueRestore{model: expectedModel, numberPresent: saved.NumberPresent, budget: budget}
	var err error
	work.strings, err = newLegacyNativeCheckpointStringValuesRestore(ctx, saved.Values, store, sourceBytes, budget-planned.bytes)
	if err != nil {
		work.close()
		return nil, err
	}
	if _, err := work.progress(ctx); err != nil {
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeCheckpointNestedValueRestore) step(ctx context.Context) (bool, error) {
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
		return work.progress(ctx)
	}
	if work.strings == nil {
		return work.fail(ErrIntegrity)
	}
	ready, err := work.strings.step(ctx)
	if err != nil {
		return work.fail(err)
	}
	if !ready {
		return work.progress(ctx)
	}
	work.values, err = work.strings.take(ctx)
	if err != nil {
		return work.fail(err)
	}
	work.strings = nil
	if err := work.materialize(ctx); err != nil {
		return work.fail(err)
	}
	work.ready = true
	return work.progress(ctx)
}

func (work *legacyNativeCheckpointNestedValueRestore) materialize(ctx context.Context) error {
	_, count, err := legacyNativeCheckpointNestedValueSchema(work.model)
	if err != nil || work.values.Values == nil || len(work.values.Values) != count || cap(work.values.Values) != count ||
		len(work.values.Capacities) != count || cap(work.values.Capacities) != count || work.values.Payload.err != nil {
		return ErrIntegrity
	}
	var payload docRangeHeaderHeapEstimate
	for ordinal, text := range work.values.Values {
		if work.values.Capacities[ordinal] != len(text) || uint64(len(text)) > legacyNativeValueCheckpointPayloadLimit() {
			return ErrIntegrity
		}
		payload.allocation(int64(len(text)), 1)
	}
	if payload.err != nil || payload.bytes != work.values.Payload.bytes ||
		work.model == legacyNativeCheckpointProviderModel && !work.numberPresent && len(work.values.Values[1]) != 0 {
		return ErrIntegrity
	}
	storage := legacyNativeCheckpointNestedValueStorage(work.model, work.numberPresent)
	if err := work.checkBudget(storage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if work.model == legacyNativeCheckpointLockedModel {
		work.result = reflect.ValueOf(&LockedBlob{
			Provider: ProviderKind(work.values.Values[0]), Scheme: work.values.Values[1], Blob: work.values.Values[2],
		})
	} else {
		value := &ProviderMetadata{TurnID: work.values.Values[0]}
		if work.numberPresent {
			number := json.Number(work.values.Values[1])
			value.CreateTime = &number
		}
		work.result = reflect.ValueOf(value)
	}
	heap, err := legacyNativeValueHeap(ctx, work.result)
	if err != nil {
		return err
	}
	storage.include(payload)
	if heap.err != nil || storage.err != nil || heap.bytes != storage.bytes {
		return ErrIntegrity
	}
	work.heap = heap
	work.values = legacyNativeCheckpointStringValues{}
	return nil
}

func (work *legacyNativeCheckpointNestedValueRestore) progress(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.checkBudget(docRangeHeaderHeapEstimate{}); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointNestedValueRestore) checkBudget(additional docRangeHeaderHeapEstimate) error {
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	return legacyNativeCheckpointStringValuesBudget(retained, additional, work.budget)
}

func (work *legacyNativeCheckpointNestedValueRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(cap(work.values.Values)), int64(unsafe.Sizeof(string(""))))
	heap.allocation(int64(cap(work.values.Capacities)), int64(unsafe.Sizeof(int(0))))
	heap.include(work.values.Payload)
	heap.include(work.heap)
	retained, err := work.strings.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointNestedValueRestore) take(ctx context.Context) (reflect.Value, error) {
	if work == nil {
		return reflect.Value{}, ErrIntegrity
	}
	if work.failure != nil {
		return reflect.Value{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return reflect.Value{}, work.failure
	}
	if work.closed || !work.ready || !work.result.IsValid() {
		work.fail(ErrIntegrity)
		return reflect.Value{}, work.failure
	}
	if _, err := work.progress(ctx); err != nil {
		return reflect.Value{}, err
	}
	result := work.result
	work.close()
	return result, nil
}

func (work *legacyNativeCheckpointNestedValueRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointNestedValueRestore) close() {
	if work != nil {
		work.strings.close()
		failure := work.failure
		*work = legacyNativeCheckpointNestedValueRestore{closed: true, failure: failure}
	}
}
