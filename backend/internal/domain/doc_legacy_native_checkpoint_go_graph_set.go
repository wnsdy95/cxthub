package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"reflect"
	"strings"
	"unsafe"
)

type legacyNativeCheckpointGoGraphRoots []legacyNativeCheckpointValue

func (roots *legacyNativeCheckpointGoGraphRoots) UnmarshalJSON(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	first, err := decoder.Token()
	if err != nil || first != json.Delim('[') {
		return ErrIntegrity
	}
	values := make(legacyNativeCheckpointGoGraphRoots, 0, 32)
	for decoder.More() {
		if len(values) == 32 {
			return ErrIntegrity
		}
		var value legacyNativeCheckpointValue
		if err := decoder.Decode(&value); err != nil {
			return ErrIntegrity
		}
		values = append(values, value)
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim(']') || len(values) == 0 {
		return ErrIntegrity
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrIntegrity
	}
	*roots = values
	return nil
}

type legacyNativeCheckpointGoGraphSetState struct {
	Version uint8                              `json:"version"`
	Binding ContentHash                        `json:"binding"`
	Roots   legacyNativeCheckpointGoGraphRoots `json:"roots"`
}

func (state legacyNativeCheckpointGoGraphSetState) validate(binding ContentHash) error {
	if state.Version != 1 || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || state.Binding != binding || len(state.Roots) == 0 || len(state.Roots) > 32 || cap(state.Roots) > 32 {
		return ErrIntegrity
	}
	return nil
}

func (state legacyNativeCheckpointGoGraphSetState) retained() docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(int64(len(state.Binding)), 1)
	heap.allocation(int64(cap(state.Roots)), int64(unsafe.Sizeof(legacyNativeCheckpointValue{})))
	for _, value := range state.Roots {
		legacyNativeCheckpointGraphCapturedValueHeap(&heap, value)
	}
	return heap
}

func legacyNativeCheckpointGoGraphSetSame(left, right any) bool {
	if reflect.TypeOf(left) != reflect.TypeOf(right) {
		return false
	}
	switch current := left.(type) {
	case nil:
		return true
	case bool:
		return current == right.(bool)
	case float64:
		return !math.IsNaN(current) && !math.IsInf(current, 0) && math.Float64bits(current) == math.Float64bits(right.(float64))
	case string:
		other := right.(string)
		return len(current) == len(other) && unsafe.StringData(current) == unsafe.StringData(other)
	case map[string]any:
		other := right.(map[string]any)
		return (current == nil) == (other == nil) && len(current) == len(other) && reflect.ValueOf(current).Pointer() == reflect.ValueOf(other).Pointer()
	case []any:
		other := right.([]any)
		return (current == nil) == (other == nil) && len(current) == len(other) && cap(current) == cap(other) && reflect.ValueOf(current).Pointer() == reflect.ValueOf(other).Pointer()
	default:
		return false
	}
}

func legacyNativeCheckpointGoGraphSetLeafHeap(value legacyNativeAnyValue) docRangeHeaderHeapEstimate {
	if value.owner == (legacyNativeAnyValueOwner{}) {
		return value.heap
	}
	return docRangeHeaderHeapEstimate{}
}

func legacyNativeCheckpointGoGraphSetSourceHeap(values []any) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	heap.allocation(int64(cap(values)), int64(unsafe.Sizeof(any(nil))))
	for _, value := range values {
		switch current := value.(type) {
		case string:
			heap.allocation(int64(len(current)), 1)
		case map[string]any:
			if current != nil {
				heap.include(legacyNativeAnyMapBacking(len(current), int64(unsafe.Sizeof(any(nil)))))
			}
		case []any:
			heap.include(legacyNativeAnyArrayBacking(cap(current)))
		}
	}
	return heap
}

type legacyNativeCheckpointGoGraphSet struct {
	owner      *legacyNativeObjectCheckpoint
	record     *legacyNativeEventRecordFieldsCheckpoint
	state      legacyNativeCheckpointGoGraphSetState
	borrowed   []any
	values     []any
	registry   *legacyNativeCheckpointGoGraphRegistry
	identity   *legacyNativeCheckpointGraphIdentity
	prepare    *legacyNativeCheckpointGoGraph
	layout     *legacyNativeCheckpointGoLayout
	write      *legacyNativeCheckpointGraphWriter
	value      legacyNativeAnyValue
	store      legacyNativeCheckpointIO
	ref        legacyNativeCheckpointRef
	position   int
	phase      uint8
	budget     int64
	ready      bool
	closed     bool
	failure    error
	sourceHeap docRangeHeaderHeapEstimate
}

func newLegacyNativeCheckpointGoGraphSet(ctx context.Context, values []any, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyNativeCheckpointGoGraphSet, error) {
	return newLegacyNativeCheckpointGoGraphSetForSource(ctx, values, binding, store, budget, nil, nil)
}

func newLegacyNativeCheckpointGoGraphSetForSource(ctx context.Context, values []any, binding ContentHash, store legacyNativeCheckpointIO, budget int64, owner *legacyNativeObjectCheckpoint, record *legacyNativeEventRecordFieldsCheckpoint) (*legacyNativeCheckpointGoGraphSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if record != nil {
		if owner != nil || record.closed || record.ready || record.failure != nil || record.graph != nil ||
			record.record.heap.err != nil || record.record.heap.bytes < 0 || record.state.Binding != binding ||
			len(values) != len(record.roots) || cap(values) != len(record.roots) || unsafe.SliceData(values) != &record.roots[0] {
			return nil, ErrConflict
		}
		if err := record.checkSource(); err != nil {
			return nil, err
		}
	}
	if owner != nil {
		if err := owner.checkInitialGraphSource(); err != nil {
			return nil, err
		}
		if binding != owner.state.Binding || len(values) != len(owner.initialValues) || cap(values) != cap(owner.initialValues) || unsafe.SliceData(values) != unsafe.SliceData(owner.initialValues) {
			return nil, ErrConflict
		}
		for index, value := range values {
			if !legacyNativeCheckpointGoGraphSetSame(value, owner.initialValues[index]) {
				return nil, ErrConflict
			}
		}
	}
	if len(values) == 0 || len(values) > 32 || cap(values) > 32 || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || store.Put == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	for _, value := range values[len(values):cap(values)] {
		if value != nil {
			return nil, ErrIntegrity
		}
	}
	for _, value := range values {
		if !legacyNativeCheckpointGoGraphSetSame(value, value) {
			return nil, ErrIntegrity
		}
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGoGraphSet{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGoGraphRegistry{})))
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphIdentity{})))
	if owner == nil && record == nil {
		planned.include(legacyNativeCheckpointGoGraphSetSourceHeap(values))
	}
	planned.allocation(int64(len(values)), int64(unsafe.Sizeof(any(nil))+unsafe.Sizeof(legacyNativeCheckpointValue{})))
	planned.allocation(int64(len(binding)), 1)
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	if owner != nil {
		if err := owner.admit(planned.bytes); err != nil {
			return nil, err
		}
	}
	work := &legacyNativeCheckpointGoGraphSet{owner: owner, record: record, borrowed: values, values: make([]any, len(values)), state: legacyNativeCheckpointGoGraphSetState{Version: 1, Binding: ContentHash(strings.Clone(string(binding))), Roots: make(legacyNativeCheckpointGoGraphRoots, len(values))}, registry: newLegacyNativeCheckpointGoGraphRegistry(), identity: newLegacyNativeCheckpointGraphIdentity(), store: store, budget: budget}
	copy(work.values, values)
	if record != nil {
		record.graph = work
		work.registry.source, work.registry.capture = work, record.capture
	} else if owner == nil {
		work.sourceHeap = legacyNativeCheckpointGoGraphSetSourceHeap(values)
	} else {
		owner.initialTask = work
		work.registry.source = work
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		if owner != nil {
			owner.initialTask = nil
		}
		if record != nil {
			record.graph = nil
		}
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeCheckpointGoGraphSet) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(cap(work.values)), int64(unsafe.Sizeof(any(nil))))
	heap.include(work.state.retained())
	heap.include(work.sourceHeap)
	heap.allocation(int64(len(work.ref.Hash)), 1)
	if work.record == nil {
		heap.include(legacyNativeCheckpointGoGraphSetLeafHeap(work.value))
	}
	for _, owner := range []interface{ retainedBytes() (int64, error) }{work.registry, work.identity, work.prepare, work.write, work.layout} {
		if owner != nil {
			retained, err := owner.retainedBytes()
			heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		}
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointGoGraphSet) remaining() (int64, error) {
	if work.record != nil {
		if err := work.checkOwnerScope(); err != nil {
			return 0, err
		}
		return work.record.remaining()
	}
	if work.owner != nil {
		if err := work.checkOwnerScope(); err != nil {
			return 0, err
		}
		return work.owner.remaining()
	}
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained < 0 || retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNativeCheckpointGoGraphSet) admit(extra int64) error {
	remaining, err := work.remaining()
	if err != nil {
		return err
	}
	if extra < 0 {
		return ErrIntegrity
	}
	if extra > remaining {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeCheckpointGoGraphSet) checkSource() error {
	if work.registry != nil && work.registry.capture != nil && !work.registry.capture.valid(work.registry.capture.scope) {
		return ErrConflict
	}
	if err := work.checkOwner(); err != nil {
		return err
	}
	if len(work.borrowed) != len(work.values) {
		return ErrConflict
	}
	for _, value := range work.borrowed[len(work.borrowed):cap(work.borrowed)] {
		if value != nil {
			return ErrConflict
		}
	}
	for index, value := range work.values {
		if !legacyNativeCheckpointGoGraphSetSame(value, work.borrowed[index]) {
			return ErrConflict
		}
	}
	return nil
}

func legacyNativeCheckpointGoGraphSetTrivial(value any) bool {
	switch value := value.(type) {
	case nil, bool:
		return true
	case float64:
		return !math.IsNaN(value) && !math.IsInf(value, 0)
	case string:
		return value == ""
	default:
		return false
	}
}

func (work *legacyNativeCheckpointGoGraphSet) trivialRoot(ctx context.Context) (bool, error) {
	if !legacyNativeCheckpointGoGraphSetTrivial(work.values[work.position]) {
		return false, nil
	}
	if work.registry.first != nil || work.identity.first != nil {
		return false, nil
	}
	if work.registry.failure != nil {
		return false, work.registry.failure
	}
	if work.identity.failure != nil {
		return false, work.identity.failure
	}
	if work.registry.closed || work.identity.closed || work.identity.nextID >= legacyNativeCheckpointNodes {
		return false, ErrIntegrity
	}
	if work.registry.active != nil {
		return false, ErrConflict
	}
	if err := work.admit(64 + 2*legacyNativeValueCheckpointBytes); err != nil {
		return false, err
	}
	bounds := legacyNativeValueCheckpointBounds{PayloadBytes: legacyNativeValueCheckpointPayloadLimit(), Nodes: legacyNativeCheckpointNodes}
	snapshot, err := (legacyNativeAnyValue{value: work.values[work.position]}).checkpoint(ctx, 0, bounds)
	if err != nil {
		return false, err
	}
	defer snapshot.close()
	wire, err := snapshot.state.encode(ctx, bounds)
	if err != nil {
		return false, err
	}
	work.identity.nextID++
	work.state.Roots[work.position] = legacyNativeCheckpointValue{Wire: wire}
	work.position++
	return true, nil
}

func (work *legacyNativeCheckpointGoGraphSet) advance(ctx context.Context) error {
	if work.phase == 0 && work.position < len(work.values) && work.prepare == nil &&
		(work.layout != nil && work.layout.ready || !legacyNativeCheckpointGoGraphSetTrivial(work.values[work.position])) {
		if handled, err := work.trivialRoot(ctx); handled || err != nil {
			return err
		}
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		shared, err := work.registry.retainedBytes()
		if err != nil {
			return err
		}
		work.prepare, err = newLegacyNativeCheckpointGoGraph(ctx, work.values[work.position], work.registry, remaining+shared)
		return err
	}
	if work.layout == nil {
		var planned docRangeHeaderHeapEstimate
		planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGoLayout{})))
		if planned.err != nil {
			return planned.err
		}
		if err := work.admit(planned.bytes); err != nil {
			return err
		}
		work.layout = &legacyNativeCheckpointGoLayout{parent: work, roots: work.values}
		work.registry.layout = work.layout
		return nil
	}
	if !work.layout.ready {
		return work.layout.step(ctx)
	}
	if work.position == len(work.values) {
		if err := work.state.validate(work.state.Binding); err != nil {
			return err
		}
		if err := work.admit(2 * LegacyCanonicalEncodingBytes); err != nil {
			return err
		}
		ref, err := legacyNativeCheckpointWriteJSON(ctx, work.store, work.state)
		if err != nil {
			return err
		}
		work.ref, work.ready = ref, true
		return nil
	}
	if work.phase == 0 {
		ready, err := work.prepare.step(ctx)
		if err != nil || !ready {
			return err
		}
		value, err := work.prepare.take(ctx)
		work.prepare = nil
		if err != nil {
			return err
		}
		work.value, work.phase = value, 1
		return nil
	}
	if work.write == nil {
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		shared, err := work.identity.retainedBytes()
		if err != nil {
			return err
		}
		work.write, err = newLegacyNativeCheckpointGraphWriter(ctx, work.value, work.store, work.identity, remaining+shared)
		return err
	}
	ready, err := work.write.step(ctx)
	if err != nil || !ready {
		return err
	}
	saved, err := work.write.take(ctx)
	work.write = nil
	if err != nil {
		return err
	}
	work.state.Roots[work.position] = saved
	work.value, work.phase = legacyNativeAnyValue{}, 0
	work.position++
	return nil
}

func (work *legacyNativeCheckpointGoGraphSet) step(ctx context.Context) (bool, error) {
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
	if err := work.checkSource(); err != nil {
		return work.fail(err)
	}
	if work.ready {
		return true, nil
	}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		return work.fail(err)
	}
	if err := work.advance(ctx); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointGoGraphSet) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
	if work == nil {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointRef{}, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if err := work.checkSource(); err != nil {
		work.fail(err)
		return legacyNativeCheckpointRef{}, err
	}
	if work.owner != nil || work.record != nil {
		if err := work.admit(0); err != nil {
			work.fail(err)
			return legacyNativeCheckpointRef{}, err
		}
	}
	ref := work.ref
	work.close()
	return ref, nil
}

func (work *legacyNativeCheckpointGoGraphSet) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointGoGraphSet) close() {
	if work != nil {
		for _, owner := range []interface{ close() }{work.prepare, work.write, work.registry, work.identity, work.layout} {
			if owner != nil {
				owner.close()
			}
		}
		failure := work.failure
		*work = legacyNativeCheckpointGoGraphSet{closed: true, failure: failure}
	}
}

type legacyNativeCheckpointGoGraphSetRestore struct {
	state    legacyNativeCheckpointGoGraphSetState
	ref      legacyNativeCheckpointRef
	binding  ContentHash
	registry *legacyNativeCheckpointGraphRestored
	read     *legacyNativeCheckpointGraphRestore
	values   []legacyNativeAnyValue
	store    legacyNativeCheckpointIO
	position int
	phase    uint8
	budget   int64
	ready    bool
	closed   bool
	failure  error
}

func newLegacyNativeCheckpointGoGraphSetRestore(ctx context.Context, ref legacyNativeCheckpointRef, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyNativeCheckpointGoGraphSetRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ref.Hash) != len(contentHashPrefix)+64 || !ref.valid() || ref.Bytes > LegacyCanonicalEncodingBytes || len(binding) != len(contentHashPrefix)+64 || ValidateContentHash(binding) != nil || store.Read == nil || budget <= 0 {
		return nil, ErrIntegrity
	}
	var planned docRangeHeaderHeapEstimate
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGoGraphSetRestore{})))
	planned.allocation(int64(len(ref.Hash)+len(binding)), 1)
	planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGraphRestored{})))
	planned.allocation(legacyNativeAnyCheckpointScratch, 1)
	if planned.err != nil {
		return nil, planned.err
	}
	if planned.bytes > budget {
		return nil, ErrDocumentResourceLimit
	}
	ref.Hash = ContentHash(strings.Clone(string(ref.Hash)))
	work := &legacyNativeCheckpointGoGraphSetRestore{ref: ref, binding: ContentHash(strings.Clone(string(binding))), registry: newLegacyNativeCheckpointGraphRestored(), store: store, budget: budget}
	if err := work.admit(legacyNativeAnyCheckpointScratch); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyNativeCheckpointGoGraphSetRestore) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.include(work.state.retained())
	heap.allocation(int64(len(work.ref.Hash)+len(work.binding)), 1)
	heap.allocation(int64(cap(work.values)), int64(unsafe.Sizeof(legacyNativeAnyValue{})))
	for _, value := range work.values {
		heap.include(legacyNativeCheckpointGoGraphSetLeafHeap(value))
	}
	for _, owner := range []interface{ retainedBytes() (int64, error) }{work.registry, work.read} {
		if owner != nil {
			retained, err := owner.retainedBytes()
			heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
		}
	}
	return heap.bytes, heap.err
}

func (work *legacyNativeCheckpointGoGraphSetRestore) remaining() (int64, error) {
	retained, err := work.retainedBytes()
	if err != nil {
		return 0, err
	}
	if retained < 0 || retained > work.budget {
		return 0, ErrDocumentResourceLimit
	}
	return work.budget - retained, nil
}

func (work *legacyNativeCheckpointGoGraphSetRestore) admit(extra int64) error {
	remaining, err := work.remaining()
	if err != nil {
		return err
	}
	if extra < 0 {
		return ErrIntegrity
	}
	if extra > remaining {
		return ErrDocumentResourceLimit
	}
	return nil
}

func (work *legacyNativeCheckpointGoGraphSetRestore) advance(ctx context.Context) error {
	if work.phase == 0 {
		if err := legacyNativeCheckpointReadJSON(ctx, work.store, work.ref, &work.state); err != nil {
			return err
		}
		if err := work.state.validate(work.binding); err != nil {
			return err
		}
		var planned docRangeHeaderHeapEstimate
		planned.allocation(int64(len(work.state.Roots)), int64(unsafe.Sizeof(legacyNativeAnyValue{})))
		if planned.err != nil {
			return planned.err
		}
		if err := work.admit(planned.bytes); err != nil {
			return err
		}
		work.values = make([]legacyNativeAnyValue, len(work.state.Roots))
		work.phase++
		return nil
	}
	if work.position == len(work.values) {
		work.ready = true
		return nil
	}
	if work.read == nil {
		remaining, err := work.remaining()
		if err != nil {
			return err
		}
		shared, err := work.registry.retainedBytes()
		if err != nil {
			return err
		}
		work.read, err = newLegacyNativeCheckpointGraphRestore(ctx, work.state.Roots[work.position], work.store, work.registry, remaining+shared)
		return err
	}
	ready, err := work.read.step(ctx)
	if err != nil || !ready {
		return err
	}
	value, err := work.read.take(ctx)
	work.read = nil
	if err != nil {
		return err
	}
	work.values[work.position] = value
	work.position++
	return nil
}

func (work *legacyNativeCheckpointGoGraphSetRestore) step(ctx context.Context) (bool, error) {
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
	if err := work.advance(ctx); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

func (work *legacyNativeCheckpointGoGraphSetRestore) take(ctx context.Context) ([]legacyNativeAnyValue, error) {
	if work == nil {
		return nil, ErrIntegrity
	}
	if work.failure != nil {
		return nil, work.failure
	}
	if err := ctx.Err(); err != nil {
		work.fail(err)
		return nil, err
	}
	if work.closed || !work.ready {
		work.fail(ErrIntegrity)
		return nil, ErrIntegrity
	}
	values := work.values
	work.close()
	return values, nil
}

func (work *legacyNativeCheckpointGoGraphSetRestore) fail(err error) (bool, error) {
	if work.failure == nil {
		work.close()
		work.failure = err
	}
	return false, work.failure
}

func (work *legacyNativeCheckpointGoGraphSetRestore) close() {
	if work != nil {
		for _, owner := range []interface{ close() }{work.read, work.registry} {
			if owner != nil {
				owner.close()
			}
		}
		failure := work.failure
		*work = legacyNativeCheckpointGoGraphSetRestore{closed: true, failure: failure}
	}
}
