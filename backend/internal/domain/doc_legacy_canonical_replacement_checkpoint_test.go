package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"unsafe"
)

func legacyCanonicalReplacementCheckpointFixture(test *testing.T) (*LegacyNormalizationPreparation, *legacySourceProofBuffer, int) {
	test.Helper()
	stage, source := legacyCanonicalSourceCheckpointStage(test, 'r')
	for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
		encoding := stage.pending[0].Encoding
		for index := 0; index < encoding.preparation.frames.length(); index++ {
			frame := encoding.preparation.frames.at(index)
			if frame.kind == 'r' && frame.phase == 1 {
				replacement := frame.value.(*legacyCanonicalReplacement)
				if replacement.order.work > 0 && replacement.order.phase != legacyCanonicalKeyOrderDone {
					return stage, source, index
				}
			}
		}
		if complete, err := stage.Step(context.Background()); err != nil || complete || stage.pending[0].Encoding.preparation == nil {
			test.Fatal("actual replacement did not stop during sorting", complete, err)
		}
	}
	test.Fatal("actual replacement never yielded a partial sort")
	return nil, nil, 0
}

func TestLegacyCanonicalReplacementCheckpointActualPreparationContinues(test *testing.T) {
	stage, source, index := legacyCanonicalReplacementCheckpointFixture(test)
	ctx := context.Background()
	canonical, original, err := legacyNormalizationPreparationOracle(source.body)
	if err != nil {
		test.Fatal(err)
	}
	encoding := stage.pending[0].Encoding
	frame := encoding.preparation.frames.at(index)
	replacement := frame.value.(*legacyCanonicalReplacement)
	keys := encoding.mapKeys.at(frame.mapPosition)
	order, keyWork, inspectWork, written := replacement.order, encoding.keyWork, encoding.inspectWork, stage.written
	before, err := encoding.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	want := make([]string, keys.fields.length())
	for position := range want {
		want[position] = keys.fields.at(position).key
	}
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer func() {
		if grant != nil {
			grant.release()
		}
	}()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(source.body)
	stageHeap, err := stage.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	var grantHeap docRangeHeaderHeapEstimate
	grantHeap.allocation(1, int64(unsafe.Sizeof(*grant)))
	budget := legacyDecoderCheckpointBudget - stageHeap - grantHeap.bytes
	work, err := newLegacyCanonicalReplacementCheckpoint(ctx, stage, index, grant, binding, store, budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	reads, puts, maximumPut, peakHeap := len(source.reads), 0, 0, int64(0)
	put := work.store.Put
	work.store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		puts += len(body)
		return put(ctx, hash, body)
	}
	ready, steps := false, 0
	for ; steps < legacyDecoderCheckpointSteps && !ready; steps++ {
		beforePut, position := puts, work.position
		ready, err = work.step(ctx)
		heap, heapErr := work.retainedBytes()
		peakHeap = max(peakHeap, stageHeap+grantHeap.bytes+heap)
		maximumPut = max(maximumPut, puts-beforePut)
		if err != nil || heapErr != nil || peakHeap > legacyDecoderCheckpointBudget || work.position-position > 1 || maximumPut > legacyNativeAnyCheckpointScratch {
			test.Fatal("sort checkpoint escaped its actual worker budget or quantum", err, heapErr, peakHeap, maximumPut)
		}
		if _, err := stage.Step(ctx); !errors.Is(err, ErrLegacyNormalizationBusy) {
			test.Fatal("sort checkpoint did not freeze the actual worker", err)
		}
	}
	if !ready || steps < replacement.length() || replacement.order != order || encoding.keyWork != keyWork || encoding.inspectWork != inspectWork || stage.written != written || len(source.reads) != reads {
		test.Fatal("sort capture replayed work, drained output, reread source, or did not finish")
	}
	ref, err := work.take(ctx)
	if err != nil || !ref.valid() {
		test.Fatal("sort checkpoint did not produce a durable root", ref, err)
	}
	keys.fields.clear()
	replacement.order = legacyCanonicalKeyOrder{}
	restore, err := newLegacyCanonicalReplacementRestore(ctx, replacement, ref, binding, store, budget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	ready = false
	checkedEOF := false
	for attempt := 0; attempt < legacyDecoderCheckpointSteps && !ready; attempt++ {
		beforeFields := restore.result.fields.length()
		if beforeFields == len(want) {
			retained, err := restore.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			restore.budget = retained + legacyNativeAnyCheckpointScratch
			checkedEOF = true
		}
		ready, err = restore.step(ctx)
		heap, heapErr := restore.retainedBytes()
		peakHeap = max(peakHeap, stageHeap+grantHeap.bytes+heap)
		if err != nil || heapErr != nil || peakHeap > legacyDecoderCheckpointBudget || restore.result.fields.length()-beforeFields > 1 {
			test.Fatal("sort restore escaped its actual worker budget or quantum", err, heapErr, peakHeap)
		}
	}
	result, err := restore.take(ctx)
	if err != nil || !ready || !checkedEOF || result.order != order || result.fields.length() != len(want) {
		test.Fatal("partial sorting control or permutation was not restored", err)
	}
	for position, key := range want {
		if result.fields.at(position).key != key || result.fields.at(position).childKeys != 0 {
			test.Fatal("restoration sorted again or changed a child preparation cursor", position)
		}
	}
	keys.fields, replacement.order = result.fields, result.order
	keys.heap, err = keys.fields.ownedBytes()
	if err != nil {
		test.Fatal(err)
	}
	after, err := encoding.RetainedBytes()
	if err != nil || after != before || len(source.reads) != reads || encoding.keyWork != keyWork || encoding.inspectWork != inspectWork || stage.written != written {
		test.Fatal("sort restore changed accounting or replayed semantic work", before, after, err)
	}
	for _, event := range stage.pending[1:] {
		if !legacyCanonicalInitialState(event.Encoding) {
			test.Fatal("saving a partial sort consumed a future event")
		}
	}
	grant.release()
	grant = nil
	complete := false
	for attempt := 0; attempt < legacyDecoderCheckpointSteps && !complete; attempt++ {
		complete, err = stage.Step(ctx)
		if err != nil {
			test.Fatal("restored partial sort could not finish actual preparation", err)
		}
	}
	output, err := stage.Source()
	if err != nil || !complete || output.Bytes != len(canonical) {
		test.Fatal("restored actual preparation did not reach verified output", err)
	}
	body, err := output.Read(ctx, 0, output.Bytes)
	if err != nil || !bytes.Equal(body, canonical) || HashContent(body) != original.Hash() {
		test.Fatal("resumed partial sort changed canonical output", err)
	}
	test.Logf("actual replacement=%d; save steps=%d; Put=%d; max step Put=%d; conservative aggregate peak=%d", len(want), steps, puts, maximumPut, peakHeap)
}

func TestLegacyCanonicalReplacementCheckpointFences(test *testing.T) {
	stage, source, index := legacyCanonicalReplacementCheckpointFixture(test)
	ctx := context.Background()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(source.body)
	for _, name := range []string{"cancel", "budget", "release", "owner", "native-owner", "frame", "native", "key-owner", "key", "child-keys"} {
		test.Run(name, func(test *testing.T) {
			grant, err := stage.mutation.beginCapture()
			if err != nil {
				test.Fatal(err)
			}
			defer func() {
				if grant != nil {
					grant.release()
				}
			}()
			work, err := newLegacyCanonicalReplacementCheckpoint(ctx, stage, index, grant, binding, store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			request, want := ctx, error(ErrConflict)
			switch name {
			case "cancel":
				var cancel context.CancelFunc
				request, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "budget":
				work.budget, want = 1, ErrDocumentResourceLimit
			case "release":
				grant.release()
				grant = nil
			case "owner":
				encoding := stage.pending[0].Encoding
				stage.pending[0].Encoding = &LegacyCanonicalEncoding{}
				defer func() { stage.pending[0].Encoding = encoding }()
			case "native-owner":
				encoding, owner := work.encoding, work.encoding.nativeOwner
				encoding.nativeOwner = &legacyNativeCanonicalOwner{}
				defer func() { encoding.nativeOwner = owner }()
			case "frame":
				frame := work.frame
				frame.phase++
				defer func() { frame.phase-- }()
			case "native":
				source, native := work.source, work.source.native
				source.native = &legacyNativeEventValues{}
				defer func() { source.native = native }()
			case "key-owner":
				keys, source := work.keys, work.keys.source
				keys.source = &legacyCanonicalReplacement{}
				defer func() { keys.source = source }()
			case "key":
				field := work.keys.fields.at(0)
				key := field.key
				field.key = "bad"
				defer func() { field.key = key }()
				want = ErrIntegrity
			case "child-keys":
				field := work.keys.fields.at(0)
				field.childKeys = 1
				defer func() { field.childKeys = 0 }()
				want = ErrIntegrity
			}
			if ready, err := work.step(request); ready || !errors.Is(err, want) {
				test.Fatal("checkpoint accepted invalid source or lifetime", ready, err, want)
			}
			if ready, err := work.step(ctx); ready || !errors.Is(err, want) {
				test.Fatal("failed checkpoint was reused", ready, err, want)
			}
		})
	}
}

func TestLegacyCanonicalReplacementRestoreRejectsInvalidState(test *testing.T) {
	ctx := context.Background()
	stage, source, index := legacyCanonicalReplacementCheckpointFixture(test)
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	store, objects := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(source.body)
	work, err := newLegacyCanonicalReplacementCheckpoint(ctx, stage, index, grant, binding, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	legacyDecoderCheckpointDrain(test, work, legacyDecoderCheckpointBudget)
	sourceValue, state := work.source, work.state
	ref, err := work.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"kind", "version", "binding", "count", "order", "comparison-end", "already-sorted", "duplicate", "ordinal", "hash", "budget", "cancel", "native"} {
		test.Run(name, func(test *testing.T) {
			candidate, root, want, request := state, ref, error(ErrIntegrity), ctx
			switch name {
			case "kind":
				candidate.Kind = "wrong"
			case "version":
				candidate.Version++
			case "binding":
				candidate.Binding = HashContent([]byte("another source"))
			case "count":
				candidate.Order.Count--
			case "order":
				candidate.Order.CompareOffset = -1
			case "comparison-end":
				candidate.Order.CompareOffset = 32
			case "already-sorted":
				candidate.Order = legacyCanonicalPreparedOrder(newLegacyCanonicalKeyOrder(candidate.Order.Count, true))
			case "duplicate", "ordinal":
				var rows legacyNativeCheckpointListWriter[uint64]
				defer rows.close()
				for ordinal := 0; ordinal < sourceValue.length(); ordinal++ {
					value := uint64(ordinal)
					if name == "duplicate" && ordinal == 1 {
						value = 0
					}
					if name == "ordinal" && ordinal == 0 {
						value = uint64(sourceValue.length())
					}
					if err := rows.append(ctx, store, value); err != nil {
						test.Fatal(err)
					}
				}
				candidate.Entries, err = rows.finish(ctx, store)
				if err != nil {
					test.Fatal(err)
				}
			case "hash":
				body := objects[ref.Hash]
				objects[ref.Hash] = bytes.Repeat([]byte("x"), len(body))
				defer func() { objects[ref.Hash] = body }()
				want = ErrPreparedDocRangeObject
			}
			if !reflect.DeepEqual(candidate, state) {
				root, err = legacyNativeCheckpointWriteJSON(ctx, store, candidate)
				if err != nil {
					test.Fatal(err)
				}
			}
			restore, err := newLegacyCanonicalReplacementRestore(ctx, sourceValue, root, binding, store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			if name == "budget" {
				restore.budget, want = 1, ErrDocumentResourceLimit
			}
			if name == "cancel" {
				var cancel context.CancelFunc
				request, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			if name == "native" {
				native := sourceValue.native
				sourceValue.native = &legacyNativeEventValues{}
				defer func() { sourceValue.native = native }()
				want = ErrConflict
			}
			var failure error
			for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
				ready, err := restore.step(request)
				if ready {
					test.Fatal("invalid durable sort state was accepted", name)
				}
				if err != nil {
					failure = err
					break
				}
			}
			if !errors.Is(failure, want) {
				test.Fatal("invalid sort state did not fail closed", failure, want)
			}
			if ready, err := restore.step(ctx); ready || !errors.Is(err, want) {
				test.Fatal("failed restore was reused", ready, err, want)
			}
		})
	}
}
