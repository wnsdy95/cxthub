package domain

import (
	"context"
	"crypto/sha256"
	"strings"
	"unsafe"
)

func (control LegacyNormalizationControl) validateStage() error {
	if !control.Staging() || control.StagePending <= 0 || control.StagePending > LegacyNormalizationStepEvents ||
		control.HeaderBytes != 0 || control.PrefixBytes != 0 || control.Position != 0 || control.SourceSlot != 0 || control.TargetSlot != 0 ||
		control.Width != 0 || control.Base != 0 || control.Left != 0 || control.Right != 0 || control.Segment != 0 || control.SegmentPos != 0 || control.Verified != 0 ||
		control.Files[1].Size != control.Count*legacyNormalizationRecordBytes || control.Files[2].Size != 0 || control.Files[3].Size != 0 ||
		!legacyNormalizationHashState(control.HashState, control.Written) || control.Active.Ordinal != uint64(control.Count) ||
		control.Active.Length != 0 || control.Active.Hash != ([sha256.Size]byte{}) || int64(int(control.Active.Sequence)) != control.Active.Sequence ||
		control.Active.Offset > uint64(control.Files[0].Size) || uint64(control.Written) != uint64(control.Files[0].Size)-control.Active.Offset ||
		control.Active.Offset < uint64(control.Count) || control.Count == 0 && control.Active.Offset != 0 {
		return ErrIntegrity
	}
	return nil
}

type legacyNormalizationStageEntry struct {
	Sequence int                       `json:"sequence"`
	Encoding legacyNativeCheckpointRef `json:"encoding"`
	Initial  bool                      `json:"initial,omitempty"`
}

type legacyNormalizationStageState struct {
	Kind    string                     `json:"kind"`
	Version uint8                      `json:"version"`
	Binding ContentHash                `json:"binding"`
	Events  legacyNativeCheckpointTree `json:"events"`
}

type legacyNormalizationStageCheckpoint struct {
	owner   *LegacyNormalizationPreparation
	grant   *legacyNormalizationCapture
	state   legacyNormalizationStageState
	store   legacyNativeCheckpointIO
	budget  int64
	index   int
	list    legacyNativeCheckpointListWriter[legacyNormalizationStageEntry]
	child   *legacyCanonicalPreparedCheckpoint
	initial *legacyCanonicalSourceCheckpoint
	ref     legacyNativeCheckpointRef
	ready   bool
}

func (work *legacyNormalizationStageCheckpoint) retainedBytes() (int64, error) {
	if work == nil {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(len(work.state.Binding)+len(work.state.Events.Ref.Hash)+len(work.ref.Hash)), 1)
	retained, err := work.list.retainedBytes()
	heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	if work.child != nil {
		retained, err := work.child.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.initial != nil {
		retained, err := work.initial.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNormalizationStageCheckpoint) step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if work == nil || work.owner == nil || !work.grant.valid(work.owner.mutation) || work.owner.phase != legacyNormalizationStage {
		return false, ErrConflict
	}
	if work.ready {
		return true, nil
	}
	retained, err := work.retainedBytes()
	if err != nil || retained+legacyNativeAnyCheckpointScratch > work.budget {
		return false, ErrDocumentResourceLimit
	}
	if work.index == len(work.owner.pending)-work.owner.next {
		if work.state.Events == (legacyNativeCheckpointTree{}) {
			work.state.Events, err = work.list.finish(ctx, work.store)
			return false, err
		}
		work.ref, err = legacyNativeCheckpointWriteJSON(ctx, work.store, work.state)
		work.ready = err == nil
		return work.ready, err
	}
	if work.index < 0 || work.index > len(work.owner.pending)-work.owner.next {
		return false, ErrIntegrity
	}
	event := work.owner.pending[work.owner.next+work.index]
	if event.Encoding.preparation != nil {
		if work.initial == nil {
			if work.index == 0 && work.owner.written != 0 || work.index != 0 && !legacyCanonicalInitialState(event.Encoding) {
				return false, ErrIntegrity
			}
			work.initial, err = newLegacyCanonicalSourceCheckpoint(ctx, event.Encoding, work.owner.mutation, work.state.Binding,
				work.owner.decoder.source.Bytes, work.store, work.budget-retained)
			return false, err
		}
		ready, err := work.initial.step(ctx)
		if err != nil || !ready {
			return false, err
		}
		ref, err := work.initial.take(ctx)
		work.initial = nil
		if err != nil {
			return false, err
		}
		if err := work.list.append(ctx, work.store, legacyNormalizationStageEntry{Sequence: event.Sequence, Encoding: ref, Initial: true}); err != nil {
			return false, err
		}
		work.index++
		return false, nil
	}
	if work.child == nil {
		work.child, err = newLegacyCanonicalPreparedCheckpoint(ctx, event.Encoding, work.state.Binding, work.store, work.budget-retained)
		return false, err
	}
	ready, err := work.child.step(ctx)
	if err != nil || !ready {
		return false, err
	}
	ref, err := work.child.take(ctx)
	work.child = nil
	if err != nil {
		return false, err
	}
	if err := work.list.append(ctx, work.store, legacyNormalizationStageEntry{Sequence: event.Sequence, Encoding: ref}); err != nil {
		return false, err
	}
	work.index++
	return false, nil
}

func (work *legacyNormalizationStageCheckpoint) close() {
	if work != nil {
		if work.initial != nil {
			work.initial.close()
		}
		if work.child != nil {
			work.child.close()
		}
		work.list.close()
		*work = legacyNormalizationStageCheckpoint{}
	}
}

func (capture *LegacyNormalizationCheckpointCapture) StageStep(ctx context.Context, binding ContentHash, store LegacyNormalizationParserCheckpointIO, budget int64) (ref LegacyNormalizationParserCheckpointRef, complete bool, failure error) {
	if capture == nil {
		return ref, false, ErrConflict
	}
	if !capture.parserBusy.CompareAndSwap(false, true) {
		return ref, false, ErrLegacyNormalizationBusy
	}
	defer func() {
		if failure != nil {
			capture.closed.Store(true)
		}
		if capture.closed.Load() {
			capture.release()
		}
		capture.parserBusy.Store(false)
		if capture.closed.Load() && capture.parserBusy.CompareAndSwap(false, true) {
			capture.release()
			capture.parserBusy.Store(false)
		}
	}()
	if err := capture.valid(ctx); err != nil {
		return ref, false, err
	}
	if !capture.control.Staging() || ValidateContentHash(binding) != nil || store.Put == nil || budget <= 0 ||
		capture.parserBudget != 0 && (capture.parserBinding != binding || capture.parserBudget != budget) {
		return ref, false, ErrIntegrity
	}
	remaining, err := capture.stageRemaining(budget)
	if err != nil {
		return ref, false, err
	}
	var planned docRangeHeaderHeapEstimate
	if capture.parserBudget == 0 {
		planned.allocation(int64(len(binding)), 1)
	}
	if capture.stageRef == (LegacyNormalizationParserCheckpointRef{}) {
		planned.include(docRangeHeaderHeapEstimate{bytes: legacyNativeAnyCheckpointScratch})
		if capture.stage == nil {
			planned.allocation(1, int64(unsafe.Sizeof(legacyNormalizationStageCheckpoint{})))
			planned.allocation(int64(len(binding)), 1)
			planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointListWriter[legacyNormalizationStageEntry]{})))
			planned.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointTreeBuilder{})))
		}
	}
	if planned.err != nil || planned.bytes > remaining {
		return ref, false, ErrDocumentResourceLimit
	}
	if capture.parserBudget == 0 {
		capture.parserBinding = ContentHash(strings.Clone(string(binding)))
		capture.parserBudget = budget
	}
	if capture.stageRef != (LegacyNormalizationParserCheckpointRef{}) {
		return capture.stageRef, true, nil
	}
	if capture.stage == nil {
		capture.stage = &legacyNormalizationStageCheckpoint{owner: capture.work, grant: capture.capture,
			store: legacyNativeCheckpointIO{Put: store.Put, Read: store.Read},
			state: legacyNormalizationStageState{Kind: "legacy-normalization-stage", Version: 1, Binding: capture.parserBinding}}
	}
	remaining, err = capture.stageRemaining(budget)
	if err != nil {
		return ref, false, err
	}
	retained, err := capture.stage.retainedBytes()
	if err != nil {
		return ref, false, err
	}
	capture.stage.budget = remaining + retained
	if capture.stage.initial != nil {
		retained, err := capture.stage.initial.retainedBytes()
		if err != nil {
			return ref, false, err
		}
		capture.stage.initial.budget = remaining + retained
	}
	if capture.stage.child != nil {
		retained, err := capture.stage.child.retainedBytes()
		if err != nil {
			return ref, false, err
		}
		capture.stage.child.budget = remaining + retained
	}
	ready, err := capture.stage.step(ctx)
	if current := capture.valid(ctx); current != nil {
		return ref, false, current
	}
	if err != nil {
		return ref, false, err
	}
	if _, err := capture.stageRemaining(budget); err != nil {
		return ref, false, err
	}
	if !ready {
		return ref, false, nil
	}
	root := capture.stage.ref
	capture.stage.close()
	capture.stage = nil
	capture.stageRef = LegacyNormalizationParserCheckpointRef{Hash: root.Hash, Bytes: root.Bytes}
	if _, err := capture.stageRemaining(budget); err != nil {
		return ref, false, err
	}
	return capture.stageRef, true, nil
}

func (capture *LegacyNormalizationCheckpointCapture) stageRemaining(budget int64) (int64, error) {
	retained, err := capture.work.RetainedBytes()
	if err != nil {
		return 0, err
	}
	extra, err := capture.RetainedBytes()
	if err != nil {
		return 0, err
	}
	planned := docRangeHeaderHeapEstimate{bytes: retained}
	planned.include(docRangeHeaderHeapEstimate{bytes: extra})
	if planned.err != nil || planned.bytes > budget {
		return 0, ErrDocumentResourceLimit
	}
	return budget - planned.bytes, nil
}

type legacyNormalizationStageRestore struct {
	state       legacyNormalizationStageState
	store       legacyNativeCheckpointIO
	budget      int64
	reader      *legacyCanonicalPreparedListReader[legacyNormalizationStageEntry]
	child       *legacyCanonicalPreparedRestore
	initial     *legacyCanonicalInitialRestore
	sourceBytes int
	events      []LegacyNormalizedEvent
	entry       legacyNormalizationStageEntry
	want        int
	ready       bool
}

func newLegacyNormalizationStageRestore(ctx context.Context, source LegacyNormalizationParserCheckpointSource, want, sourceBytes int, budget int64) (*legacyNormalizationStageRestore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if want <= 0 || want > LegacyNormalizationStepEvents || sourceBytes <= 0 || sourceBytes > MaxFinalizedDocBytes || budget <= legacyNativeAnyCheckpointScratch {
		return nil, ErrIntegrity
	}
	store := legacyNativeCheckpointIO{Put: source.Store.Put, Read: source.Store.Read}
	var state legacyNormalizationStageState
	if err := legacyNativeCheckpointReadJSON(ctx, store, legacyNativeCheckpointRef{Hash: source.Ref.Hash, Bytes: source.Ref.Bytes}, &state); err != nil {
		return nil, err
	}
	if state.Kind != "legacy-normalization-stage" || state.Version != 1 || state.Binding != source.Binding ||
		ValidateContentHash(state.Binding) != nil || !state.Events.valid() || state.Events.Count != uint64(want) {
		return nil, ErrIntegrity
	}
	reader, err := newLegacyCanonicalPreparedListReader[legacyNormalizationStageEntry](state.Events, store)
	if err != nil {
		return nil, err
	}
	return &legacyNormalizationStageRestore{state: state, store: store, budget: budget, reader: reader, want: want, sourceBytes: sourceBytes}, nil
}

func (work *legacyNormalizationStageRestore) retainedBytes() (int64, error) {
	if work == nil {
		return 0, nil
	}
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(*work)))
	heap.allocation(int64(cap(work.events)), int64(unsafe.Sizeof(LegacyNormalizedEvent{})))
	heap.allocation(int64(len(work.state.Binding)+len(work.state.Events.Ref.Hash)+len(work.entry.Encoding.Hash)), 1)
	for _, event := range work.events {
		retained, err := event.Encoding.RetainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.reader != nil {
		retained, err := work.reader.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.child != nil {
		retained, err := work.child.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	if work.initial != nil {
		retained, err := work.initial.retainedBytes()
		heap.include(docRangeHeaderHeapEstimate{bytes: retained, err: err})
	}
	return heap.bytes, heap.err
}

func (work *legacyNormalizationStageRestore) step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if work == nil || work.reader == nil {
		return false, ErrConflict
	}
	if work.ready {
		return true, nil
	}
	retained, err := work.retainedBytes()
	if err != nil || retained+legacyNativeAnyCheckpointScratch > work.budget {
		return false, ErrDocumentResourceLimit
	}
	if work.events == nil {
		var heap docRangeHeaderHeapEstimate
		heap.allocation(int64(work.want), int64(unsafe.Sizeof(LegacyNormalizedEvent{})))
		if heap.err != nil || heap.bytes > work.budget-retained-legacyNativeAnyCheckpointScratch {
			return false, ErrDocumentResourceLimit
		}
		work.events = make([]LegacyNormalizedEvent, 0, work.want)
		return false, nil
	}
	if work.child == nil && work.initial == nil {
		entry, found, err := work.reader.next(ctx)
		if err != nil || !found {
			if err == nil && len(work.events) != work.want {
				return false, ErrIntegrity
			}
			work.ready = err == nil
			return work.ready, err
		}
		if !entry.Encoding.valid() || len(work.events) == work.want {
			return false, ErrIntegrity
		}
		if entry.Initial {
			work.initial, err = newLegacyCanonicalInitialRestore(ctx, entry.Encoding, work.state.Binding, work.sourceBytes, work.store, work.budget-retained)
		} else {
			work.child, err = newLegacyCanonicalPreparedRestore(ctx, entry.Encoding, work.state.Binding, work.store, work.budget-retained)
		}
		if err == nil {
			work.entry = entry
		}
		return false, err
	}
	if work.initial != nil {
		ready, err := work.initial.step(ctx)
		if err != nil || !ready {
			return false, err
		}
		encoding, err := work.initial.take(ctx)
		work.initial = nil
		if err != nil {
			return false, err
		}
		work.events = append(work.events, LegacyNormalizedEvent{Sequence: work.entry.Sequence, Encoding: encoding})
		work.entry = legacyNormalizationStageEntry{}
		return false, nil
	}
	ready, err := work.child.step(ctx)
	if err != nil || !ready {
		return false, err
	}
	encoding, err := work.child.take(ctx)
	work.child = nil
	if err != nil {
		return false, err
	}
	work.events = append(work.events, LegacyNormalizedEvent{Sequence: work.entry.Sequence, Encoding: encoding})
	work.entry = legacyNormalizationStageEntry{}
	return false, nil
}

func (work *legacyNormalizationStageRestore) close() {
	if work != nil {
		if work.initial != nil {
			work.initial.close()
		}
		if work.child != nil {
			work.child.close()
		}
		if work.reader != nil {
			work.reader.close()
		}
		for _, event := range work.events {
			event.Encoding.release()
		}
		*work = legacyNormalizationStageRestore{}
	}
}

func (restore *LegacyNormalizationRestore) restoreStage(ctx context.Context) (bool, error) {
	retained, err := restore.retainedBytes()
	if err != nil || retained+legacyNativeAnyCheckpointScratch >= restore.parserBudget {
		return false, ErrDocumentResourceLimit
	}
	if restore.stage == nil {
		if restore.source.Encoder == nil {
			return false, ErrIntegrity
		}
		restore.stage, err = newLegacyNormalizationStageRestore(ctx, *restore.source.Encoder, restore.control.StagePending,
			restore.work.decoder.source.Bytes, restore.parserBudget-retained)
		return false, err
	}
	child, err := restore.stage.retainedBytes()
	if err != nil || child > retained {
		return false, ErrIntegrity
	}
	restore.stage.budget = restore.parserBudget - (retained - child)
	ready, err := restore.stage.step(ctx)
	if current := restore.current(ctx); current != nil {
		return false, current
	}
	if err != nil || !ready {
		return false, err
	}
	if len(restore.stage.events) != restore.control.StagePending || len(restore.stage.events) == 0 ||
		int64(restore.stage.events[0].Sequence) != restore.control.Active.Sequence {
		return false, ErrIntegrity
	}
	if restore.control.Written != 0 && restore.stage.events[0].Encoding.preparation != nil {
		return false, ErrIntegrity
	}
	restore.work.pending, restore.work.next = restore.stage.events, 0
	restore.stage.events = nil
	restore.stage.close()
	restore.stage, restore.source.Encoder = nil, nil
	return true, nil
}
