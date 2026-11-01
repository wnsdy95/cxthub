package domain

import (
	"context"
	"reflect"
	"unsafe"
)

type legacyCanonicalMapContinuationValueCodec interface {
	encode(context.Context, any) (legacyNativeCheckpointRef, error)
	decode(context.Context, legacyNativeCheckpointRef) (any, error)
	decodeKey(context.Context, legacyNativeCheckpointText) (string, error)
	check(context.Context, int64) error
}

type legacyCanonicalMapContinuationEntry struct {
	Key   legacyNativeCheckpointText `json:"key"`
	Value legacyNativeCheckpointRef  `json:"value"`
}

func (entry legacyCanonicalMapContinuationEntry) validate() error {
	text := entry.Key
	if !entry.Value.valid() || text.Bytes < 0 || uint64(text.Bytes) > legacyNativeValueCheckpointPayloadLimit() ||
		!text.Tree.valid() || text.Tree.Count != uint64(text.Bytes) ||
		(text.Bytes == 0) != (text.Tree == (legacyNativeCheckpointTree{})) {
		return ErrIntegrity
	}
	return nil
}

type legacyCanonicalMapContinuationState struct {
	Kind     string                     `json:"kind"`
	Version  uint8                      `json:"version"`
	Binding  ContentHash                `json:"binding"`
	Count    int                        `json:"count"`
	Position int                        `json:"position"`
	Entries  legacyNativeCheckpointTree `json:"entries"`
}

func (state legacyCanonicalMapContinuationState) validate(binding ContentHash, count, position int) error {
	if state.Kind != "map-continuation" || state.Version != legacyNativeCheckpointVersion || state.Binding != binding ||
		state.Count != count || state.Position != position || position < 0 || count < position ||
		!state.Entries.valid() || state.Entries.Count != uint64(count-position) ||
		(count == position) != (state.Entries == (legacyNativeCheckpointTree{})) {
		return ErrIntegrity
	}
	return nil
}

func legacyCanonicalMapContinuationAdmission(ctx context.Context, codec legacyCanonicalMapContinuationValueCodec, retained, extra, budget int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if codec == nil || retained < 0 || extra < 0 {
		return ErrIntegrity
	}
	heap := docRangeHeaderHeapEstimate{bytes: retained}
	heap.include(docRangeHeaderHeapEstimate{bytes: extra})
	heap.include(legacyNativeCheckpointStringValuesWorkspace())
	if heap.err != nil || budget <= 0 || heap.bytes > budget {
		return ErrDocumentResourceLimit
	}
	if err := codec.check(ctx, heap.bytes); err != nil {
		return err
	}
	return ctx.Err()
}

type legacyCanonicalMapContinuationCheckpoint struct {
	owner       *LegacyNormalizationPreparation
	encoding    *LegacyCanonicalEncoding
	nativeOwner *legacyNativeCanonicalOwner
	preparation *legacyCanonicalPreparation
	frame       *legacyCanonicalPreparationFrame
	snapshot    legacyCanonicalPreparationFrame
	resume      legacyCanonicalMapContinuation
	iterator    reflect.MapIter
	grant       *legacyNormalizationCapture
	codec       legacyCanonicalMapContinuationValueCodec
	store       legacyNativeCheckpointIO
	state       legacyCanonicalMapContinuationState
	entries     legacyNativeCheckpointListWriter[legacyCanonicalMapContinuationEntry]
	text        *legacyNativeCheckpointTextWriter
	field       legacyCanonicalKeyField
	entry       legacyCanonicalMapContinuationEntry
	ref         legacyNativeCheckpointRef
	budget      int64
	frameIndex  int
	position    int
	phase       uint8
	ready       bool
	closed      bool
	failure     error
}

func newLegacyCanonicalMapContinuationCheckpoint(ctx context.Context, owner *LegacyNormalizationPreparation, index int, grant *legacyNormalizationCapture, codec legacyCanonicalMapContinuationValueCodec, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalMapContinuationCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if owner == nil || grant == nil || !grant.valid(owner.mutation) {
		return nil, ErrConflict
	}
	if owner.terminal != nil || owner.phase != legacyNormalizationStage || owner.next < 0 || owner.next >= len(owner.pending) ||
		codec == nil || ValidateContentHash(binding) != nil || store.Put == nil {
		return nil, ErrIntegrity
	}
	encoding := owner.pending[owner.next].Encoding
	if encoding == nil || encoding.preparation == nil || index < 0 || index >= encoding.preparation.frames.length() {
		return nil, ErrIntegrity
	}
	frame := encoding.preparation.frames.at(index)
	if frame == nil || frame.kind != 'o' || frame.phase != 0 || frame.position < 0 || frame.position > frame.count ||
		(frame.iterator == nil) == (frame.mapResume == nil) {
		return nil, ErrIntegrity
	}
	source, valid := frame.value.(map[string]any)
	if !valid || source == nil || len(source) != frame.count {
		return nil, ErrIntegrity
	}
	work := &legacyCanonicalMapContinuationCheckpoint{owner: owner, encoding: encoding, nativeOwner: encoding.nativeOwner,
		preparation: encoding.preparation, frame: frame, snapshot: *frame, frameIndex: index, grant: grant, codec: codec, store: store, budget: budget,
		state: legacyCanonicalMapContinuationState{Kind: "map-continuation", Version: legacyNativeCheckpointVersion, Binding: binding, Count: frame.count, Position: frame.position}}
	if frame.iterator != nil {
		work.iterator = *frame.iterator
	} else {
		work.resume = *frame.mapResume
		if !work.resume.fields.valid() || work.resume.position < 0 || work.resume.position > work.resume.fields.length() ||
			work.resume.fields.length()-work.resume.position != frame.count-frame.position {
			work.close()
			return nil, ErrIntegrity
		}
	}
	if err := work.current(ctx); err != nil {
		work.close()
		return nil, err
	}
	return work, nil
}

func (work *legacyCanonicalMapContinuationCheckpoint) sourceCurrent() error {
	if work == nil || work.closed || work.owner == nil || !work.grant.valid(work.owner.mutation) {
		return ErrConflict
	}
	if work.owner.terminal != nil || work.owner.phase != legacyNormalizationStage || work.owner.next < 0 || work.owner.next >= len(work.owner.pending) ||
		work.owner.pending[work.owner.next].Encoding != work.encoding || work.encoding.nativeOwner != work.nativeOwner ||
		work.encoding.preparation != work.preparation || work.preparation.frames.at(work.frameIndex) != work.frame {
		return ErrConflict
	}
	frame, snapshot := work.frame, work.snapshot
	if !legacyNativeCheckpointGoGraphSetSame(frame.value, snapshot.value) || len(frame.value.(map[string]any)) != work.state.Count ||
		frame.iterator != snapshot.iterator || frame.mapResume != snapshot.mapResume || frame.position != snapshot.position ||
		frame.kind != snapshot.kind || frame.phase != snapshot.phase || frame.count != snapshot.count || frame.prepare != snapshot.prepare ||
		frame.depth != snapshot.depth || frame.length != snapshot.length || frame.mapPosition != snapshot.mapPosition || frame.keyBytes != snapshot.keyBytes ||
		frame.heap != snapshot.heap || frame.mapResume != nil && *frame.mapResume != work.resume {
		return ErrConflict
	}
	return nil
}

func (work *legacyCanonicalMapContinuationCheckpoint) current(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := work.sourceCurrent(); err != nil {
		return err
	}
	retained, err := work.retainedBytes()
	if err != nil {
		return err
	}
	if err := legacyCanonicalMapContinuationAdmission(ctx, work.codec, retained, 0, work.budget); err != nil {
		return err
	}
	return work.sourceCurrent()
}

func (work *legacyCanonicalMapContinuationCheckpoint) guardedStore() legacyNativeCheckpointIO {
	return legacyNativeCheckpointIO{Put: func(ctx context.Context, hash ContentHash, body []byte) error {
		if err := work.current(ctx); err != nil {
			return err
		}
		if err := work.store.Put(ctx, hash, body); err != nil {
			return err
		}
		return work.current(ctx)
	}}
}

func (work *legacyCanonicalMapContinuationCheckpoint) retainedBytes() (int64, error) {
	if work == nil || work.closed {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(1, 128)
	heap.allocation(int64(len(work.state.Binding)+len(work.state.Entries.Ref.Hash)+len(work.entry.Key.Tree.Ref.Hash)+len(work.entry.Value.Hash)+len(work.ref.Hash)), 1)
	retained, err := work.entries.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	retained, err = work.text.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	return heap.bytes, heap.err
}

func (work *legacyCanonicalMapContinuationCheckpoint) advance(ctx context.Context) error {
	switch work.phase {
	case 0:
		if work.position == work.state.Count-work.state.Position {
			if work.snapshot.iterator != nil && work.iterator.Next() {
				return ErrIntegrity
			}
			work.phase = 5
			return nil
		}
		if work.snapshot.iterator != nil {
			if !work.iterator.Next() {
				return ErrIntegrity
			}
			work.field = legacyCanonicalKeyField{key: work.iterator.Key().String(), value: work.iterator.Value().Interface()}
		} else {
			field := work.resume.fields.at(work.resume.position + work.position)
			if field == nil || field.childKeys != 0 {
				return ErrIntegrity
			}
			work.field = *field
		}
		work.phase = 1
	case 1:
		ref, err := work.codec.encode(ctx, work.field.value)
		if err != nil {
			return err
		}
		if !ref.valid() {
			return ErrIntegrity
		}
		work.entry.Value, work.phase = ref, 2
	case 2:
		key := work.field.key
		text, err := newLegacyNativeCheckpointTextWriter(ctx, len(key), func(ctx context.Context, offset, length int) ([]byte, error) {
			if err := work.current(ctx); err != nil {
				return nil, err
			}
			if offset < 0 || length <= 0 || length > LegacyCanonicalEncodingBytes || offset > len(key) || length > len(key)-offset {
				return nil, ErrIntegrity
			}
			return []byte(key[offset : offset+length]), nil
		}, work.guardedStore())
		if err != nil {
			return err
		}
		work.text, work.phase = text, 3
	case 3:
		ready, err := work.text.step(ctx)
		if err != nil || !ready {
			return err
		}
		text, err := work.text.take(ctx)
		if err != nil {
			return err
		}
		work.entry.Key, work.text, work.phase = text, nil, 4
	case 4:
		if err := work.entry.validate(); err != nil {
			return err
		}
		if err := work.entries.append(ctx, work.guardedStore(), work.entry); err != nil {
			return err
		}
		work.position++
		work.field, work.entry, work.phase = legacyCanonicalKeyField{}, legacyCanonicalMapContinuationEntry{}, 0
	case 5:
		tree, err := work.entries.finish(ctx, work.guardedStore())
		if err != nil {
			return err
		}
		work.state.Entries, work.phase = tree, 6
	case 6:
		if err := work.state.validate(work.state.Binding, work.snapshot.count, work.snapshot.position); err != nil {
			return err
		}
		ref, err := legacyNativeCheckpointWriteJSON(ctx, work.guardedStore(), work.state)
		if err != nil {
			return err
		}
		work.ref, work.ready = ref, true
	default:
		return ErrIntegrity
	}
	return nil
}

func (work *legacyCanonicalMapContinuationCheckpoint) step(ctx context.Context) (ready bool, failure error) {
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	defer func() {
		if failure != nil {
			work.close()
			work.failure = failure
		}
	}()
	if err := work.current(ctx); err != nil {
		return false, err
	}
	if !work.ready {
		if err := work.advance(ctx); err != nil {
			return false, err
		}
	}
	if err := work.current(ctx); err != nil {
		return false, err
	}
	return work.ready, nil
}

func (work *legacyCanonicalMapContinuationCheckpoint) take(ctx context.Context) (legacyNativeCheckpointRef, error) {
	if work == nil {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	if work.failure != nil {
		return legacyNativeCheckpointRef{}, work.failure
	}
	if err := work.current(ctx); err != nil {
		work.close()
		work.failure = err
		return legacyNativeCheckpointRef{}, err
	}
	if !work.ready {
		return legacyNativeCheckpointRef{}, ErrDocRangeIndexPending
	}
	ref := work.ref
	work.close()
	return ref, nil
}

func (work *legacyCanonicalMapContinuationCheckpoint) close() {
	if work != nil && !work.closed {
		work.text.close()
		work.entries.close()
		*work = legacyCanonicalMapContinuationCheckpoint{closed: true, failure: work.failure}
	}
}
