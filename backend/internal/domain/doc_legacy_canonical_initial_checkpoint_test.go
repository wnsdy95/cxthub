package domain

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyCanonicalInitialCheckpointStage(test *testing.T, events string) (*LegacyNormalizationPreparation, *legacySourceProofBuffer) {
	test.Helper()
	buffer := &legacySourceProofBuffer{body: []byte(`{"envelope":{"cir_version":"2","source_provider":"codex","fidelity":"full"},"events":[` + events + `]}`)}
	stage, err := NewLegacyNormalizationPreparation(context.Background(), buffer.input(), &legacyNormalizationPreparationStorage{test: test})
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		for _, event := range stage.pending {
			if event.Encoding != nil {
				event.Encoding.release()
			}
		}
		stage.Close()
	})
	for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
		complete, err := stage.Step(context.Background())
		if err != nil || complete {
			test.Fatal("preparation did not reach its actual decoded batch", complete, err)
		}
		if stage.phase == legacyNormalizationStage {
			if len(stage.pending) == 0 || stage.next != 0 {
				test.Fatal("decoded batch has no initial event")
			}
			for _, event := range stage.pending {
				if !legacyCanonicalInitialState(event.Encoding) {
					test.Fatal("decoding advanced a future event's canonical preparation")
				}
			}
			return stage, buffer
		}
	}
	test.Fatal("actual decoded batch did not become available")
	return nil, nil
}

func legacyCanonicalInitialCheckpointRoot(test *testing.T, stage *LegacyNormalizationPreparation, buffer *legacySourceProofBuffer, store legacyNativeCheckpointIO) legacyNativeCheckpointRef {
	test.Helper()
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	work, err := newLegacyCanonicalInitialCheckpoint(context.Background(), stage.pending[0].Encoding, stage.mutation, HashContent(buffer.body), len(buffer.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	legacyDecoderCheckpointDrain(test, work, legacyDecoderCheckpointBudget)
	if ready, err := work.step(context.Background()); err != nil || !ready {
		test.Fatal("ready checkpoint is not stable", ready, err)
	}
	ref, err := work.take(context.Background())
	if err != nil || !ref.valid() || grant.references.Load() != 1 {
		test.Fatal("initial checkpoint did not release its nested capture references", ref, err, grant.references.Load())
	}
	return ref
}

func TestLegacyCanonicalInitialCheckpointActualStageRoundTrip(test *testing.T) {
	large := strings.Repeat("초기🙂", LegacyCanonicalEncodingBytes/4)
	for _, fixture := range []struct {
		name  string
		event string
	}{
		{name: "message-hidden-blocks", event: `{"kind":"message","seq":7,"role":"user","blocks":[{"type":"text","text":"hidden-first"},{"type":"text","text":"` + large + `"}],"blocks":[{"type":"text","text":"visible"}],"input":{"omitted":[true,null,3]}}`},
		{name: "nested-hidden-replacement", event: `{"kind":"compaction","seq":8,"replacement_complete":true,"replacement":[{"kind":"turn","seq":3},{"kind":"message","seq":1,"role":"user","blocks":[{"text":"hidden"}]}],"replacement":[{"kind":"compaction","seq":2,"replacement_complete":true,"replacement":[{"kind":"turn","seq":4},{"kind":"turn","seq":1}]}]}`},
		{name: "tool-input", event: `{"kind":"tool_call","seq":9,"tool_name":"test","call_id":"call","input":{"nested":[true,null,{"payload":"` + large + `"}],"number":0.0000001}}`},
		{name: "tool-output", event: `{"kind":"tool_result","seq":10,"call_id":"call","is_error":false,"output":{"nested":[false,null,4]},"provider_metadata":{"turn_id":"turn","create_time":1e-7},"cross_replayable":true}`},
		{name: "nil-containers", event: `{"kind":"turn","seq":11,"blocks":null,"replacement":null,"output":null}`},
		{name: "empty-containers", event: `{"kind":"compaction","seq":12,"replacement_complete":true,"replacement":[],"blocks":[]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			stage, buffer := legacyCanonicalInitialCheckpointStage(test, fixture.event+`,{"kind":"turn","seq":99}`)
			encoding := stage.pending[0].Encoding
			owner, preparation := encoding.nativeOwner, encoding.preparation
			before := *encoding
			retained, err := encoding.RetainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			reads := len(buffer.reads)
			store, _ := legacyNativeCheckpointStoreFixture(test)
			ref := legacyCanonicalInitialCheckpointRoot(test, stage, buffer, store)
			if !reflect.DeepEqual(*encoding, before) || encoding.nativeOwner != owner || encoding.preparation != preparation {
				test.Fatal("capture advanced or replaced its borrowed canonical state")
			}
			after, err := encoding.RetainedBytes()
			if err != nil || after != retained {
				test.Fatal("capture changed source retention", retained, after, err)
			}
			native, err := newLegacyNativeEventValuesCheckpointRestore(context.Background(), ref, HashContent(buffer.body), len(buffer.body), store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal("initial root is not an ordinary native event-values checkpoint", err)
			}
			defer native.close()
			legacyDecoderCheckpointDrain(test, native, legacyDecoderCheckpointBudget)
			payload, err := native.take(context.Background())
			if err != nil || payload.Values == nil || payload.Values.count != 1 || payload.Values.records.length() != 1 {
				test.Fatal("native checkpoint did not preserve the singleton adapter", err)
			}
			record := payload.Values.records.at(0)
			if !reflect.DeepEqual(record.event, owner.event) {
				test.Fatal("native checkpoint changed typed event fields or presence")
			}
			legacyEventValuesCheckpointCompare(test, owner.replacement, record.replacement)
			if owner.blocks == nil || record.blocks == nil {
				if owner.blocks != nil || record.blocks != nil {
					test.Fatal("native checkpoint changed nil block shape")
				}
			} else {
				if owner.blocks.count != record.blocks.count || owner.blocks.records.length() != record.blocks.records.length() {
					test.Fatal("native checkpoint lost hidden block records")
				}
				for index := 0; index < owner.blocks.records.length(); index++ {
					if owner.blocks.records.at(index).block != record.blocks.records.at(index).block {
						test.Fatal("native checkpoint changed block payload", index)
					}
				}
			}
			restore, err := newLegacyCanonicalInitialRestore(context.Background(), ref, HashContent(buffer.body), len(buffer.body), store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			legacyDecoderCheckpointDrain(test, restore, legacyDecoderCheckpointBudget)
			restored, err := restore.take(context.Background())
			if err != nil || !legacyCanonicalInitialState(restored) || restored.nativeOwner == owner || restored.preparation == preparation {
				test.Fatal("restore did not hand off an independent, unvisited preparation", err)
			}
			defer restored.release()
			if restored.nativeOwner.heap != record.heap || len(buffer.reads) != reads {
				test.Fatal("restore charged adapter metadata as payload or reread original source")
			}
			for _, event := range stage.pending {
				if !legacyCanonicalInitialState(event.Encoding) {
					test.Fatal("checkpoint or restore prepared a future event")
				}
			}
			scope := &legacyNormalizationMutation{}
			grant, err := scope.beginCapture()
			if err != nil {
				test.Fatal(err)
			}
			defer grant.release()
			recapture, err := newLegacyCanonicalInitialCheckpoint(context.Background(), restored, scope, HashContent(buffer.body), len(buffer.body), store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal("restored initial preparation cannot be captured again", err)
			}
			defer recapture.close()
			legacyDecoderCheckpointDrain(test, recapture, legacyDecoderCheckpointBudget)
			if _, err := recapture.take(context.Background()); err != nil {
				test.Fatal(err)
			}
			actual, _ := collectLegacyCanonicalEncoding(test, restored)
			expected, _ := collectLegacyCanonicalEncoding(test, encoding)
			if !bytes.Equal(actual, expected) || HashContent(actual) != HashContent(expected) {
				test.Fatal("restoration changed canonical bytes or hash")
			}
		})
	}
}

func TestLegacyCanonicalInitialCheckpointAdditionalHeadroom(test *testing.T) {
	stage, buffer := legacyCanonicalInitialCheckpointStage(test, `{"kind":"message","seq":1,"role":"user","blocks":[{"text":"`+strings.Repeat("x", 2<<20)+`"}]}`)
	encoding := stage.pending[0].Encoding
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	const budget int64 = 2 << 20
	work, err := newLegacyCanonicalInitialCheckpoint(context.Background(), encoding, stage.mutation, HashContent(buffer.body), len(buffer.body), store, budget)
	if err != nil {
		test.Fatal("borrowed source payload consumed the additional-headroom budget", err)
	}
	defer work.close()
	if encoding.nativeOwner.heap.bytes <= budget || work.child.capture != grant || work.list.mutation != stage.mutation || !work.list.paged || work.list.end != len(buffer.body) {
		test.Fatal("adapter is not a borrowed singleton in the frozen outer scope")
	}
	if work.list.values.payload != encoding.nativeOwner.heap || work.list.values.records.at(0).heap != encoding.nativeOwner.heap {
		test.Fatal("adapter changed the source payload ledger")
	}
	for attempt := 0; attempt < 32; attempt++ {
		retained, err := work.retainedBytes()
		child, childErr := work.child.retainedBytes()
		var expected, borrowed docRangeHeaderHeapEstimate
		expected.allocation(1, int64(unsafe.Sizeof(*work)))
		expected.allocation(1, int64(unsafe.Sizeof(*work.list)))
		expected.allocation(2, int64(unsafe.Sizeof(uintptr(0))))
		borrowed = encoding.nativeOwner.heap
		borrowed.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
		if work.child.fields != nil {
			if work.child.fields.capture != grant {
				test.Fatal("field writer allocated an independent capture grant")
			}
			borrowed.allocation(1, int64(unsafe.Sizeof(legacyNormalizationCapture{})))
		}
		expected.include(docRangeHeaderHeapEstimate{bytes: child - borrowed.bytes})
		if err != nil || childErr != nil || expected.err != nil || retained != expected.bytes || retained < work.list.values.backing().bytes || retained > budget {
			test.Fatal("additional retention omitted adapter backing or duplicated shared payload/grants", retained, expected.bytes, err, childErr)
		}
		if work.child.fields != nil {
			work.close()
			if grant.references.Load() != 1 || !legacyCanonicalInitialState(encoding) {
				test.Fatal("closing the adapter consumed its source or outer capture")
			}
			return
		}
		if ready, err := work.step(context.Background()); err != nil || ready {
			test.Fatal("native writer failed before opening its field child", ready, err)
		}
	}
	test.Fatal("native writer did not open its field child")
}

func TestLegacyCanonicalInitialRestorePayloadHandoff(test *testing.T) {
	stage, buffer := legacyCanonicalInitialCheckpointStage(test, `{"kind":"compaction","seq":1,"replacement_complete":true,"replacement":[{"kind":"turn","seq":2}],"input":{"omitted":[1,true,null]}}`)
	store, _ := legacyNativeCheckpointStoreFixture(test)
	ref := legacyCanonicalInitialCheckpointRoot(test, stage, buffer, store)
	restore, err := newLegacyCanonicalInitialRestore(context.Background(), ref, HashContent(buffer.body), len(buffer.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	legacyDecoderCheckpointDrain(test, restore.child, legacyDecoderCheckpointBudget)
	adapter := restore.child.result.Values
	record := adapter.records.at(0)
	payload := record.heap
	if payload.bytes >= adapter.heap().bytes || adapter.payload != payload {
		test.Fatal("fixture did not retain separate adapter metadata")
	}
	legacyDecoderCheckpointDrain(test, restore, legacyDecoderCheckpointBudget)
	if restore.child != nil || adapter.count != 0 || adapter.records.length() != 0 || adapter.payload != (docRangeHeaderHeapEstimate{}) || !reflect.DeepEqual(*record, legacyNativeEventValue{}) {
		test.Fatal("ready encoding still retains its old adapter references")
	}
	encoding := restore.encoding
	if !legacyCanonicalInitialState(encoding) || encoding.nativeOwner.heap != payload {
		test.Fatal("owner payload includes the retired adapter metadata")
	}
	root, ok := encoding.root.(map[string]any)
	if !ok {
		test.Fatal("restored encoding has no native projection")
	}
	expected := payload
	expected.include(legacyCanonicalNativeProjectionHeap(encoding.nativeOwner.event, root))
	if expected.err != nil || encoding.rootHeap != expected {
		test.Fatal("projection retention lost or duplicated restored payload", encoding.rootHeap, expected)
	}
	retained, err := restore.retainedBytes()
	encoded, encodingErr := encoding.RetainedBytes()
	var metadata docRangeHeaderHeapEstimate
	metadata.allocation(1, int64(unsafe.Sizeof(*restore)))
	metadata.allocation(2, int64(unsafe.Sizeof(uintptr(0))))
	if err != nil || encodingErr != nil || retained != encoded+metadata.bytes {
		test.Fatal("restore wrapper metadata is not separate from encoding retention", retained, encoded, err, encodingErr)
	}
	restoredOwner := encoding.nativeOwner
	restore.close()
	if restoredOwner.heap != (docRangeHeaderHeapEstimate{}) || restoredOwner.replacement != nil || !reflect.DeepEqual(restoredOwner.event, CIREvent{}) {
		test.Fatal("closing a ready restore retained its unclaimed payload")
	}
	legacyNativeAnyW78Released(test, restore)
}

func TestLegacyCanonicalInitialCheckpointFailureCleanup(test *testing.T) {
	for _, scenario := range []string{"cancel-child", "source-stop", "write-failure", "early-take", "budget"} {
		test.Run(scenario, func(test *testing.T) {
			stage, buffer := legacyCanonicalInitialCheckpointStage(test, `{"kind":"tool_call","seq":1,"input":{"nested":[1,true,null]}}`)
			encoding := stage.pending[0].Encoding
			grant, err := stage.mutation.beginCapture()
			if err != nil {
				test.Fatal(err)
			}
			defer grant.release()
			store, _ := legacyNativeCheckpointStoreFixture(test)
			if scenario == "write-failure" {
				store.Put = func(context.Context, ContentHash, []byte) error { return ErrConflict }
			}
			work, err := newLegacyCanonicalInitialCheckpoint(context.Background(), encoding, stage.mutation, HashContent(buffer.body), len(buffer.body), store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expected := error(ErrIntegrity)
			switch scenario {
			case "source-stop":
				stage.mutation.stop()
				expected = ErrConflict
			case "write-failure":
				expected = ErrConflict
			case "budget":
				work.budget = 1
				expected = ErrDocumentResourceLimit
			case "cancel-child":
				expected = context.Canceled
			}
			var failure error
			if scenario == "early-take" {
				_, failure = work.take(ctx)
			} else {
				for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
					if scenario == "cancel-child" && work.child != nil && work.child.fields != nil {
						cancel()
					}
					ready, err := work.step(ctx)
					if ready {
						test.Fatal("failed checkpoint exposed a root")
					}
					if err != nil {
						failure = err
						break
					}
				}
			}
			if !errors.Is(failure, expected) || grant.references.Load() != 1 || !legacyCanonicalInitialState(encoding) {
				test.Fatal("failure changed its cause, consumed source, or leaked a nested capture", failure, expected, grant.references.Load())
			}
			if _, err := work.take(context.Background()); !errors.Is(err, failure) {
				test.Fatal("failed checkpoint lost its sticky error", err)
			}
			legacyNativeAnyW78Released(test, work)
		})
	}
}

func TestLegacyCanonicalInitialRestoreFailureCleanup(test *testing.T) {
	for _, scenario := range []string{"wrong-binding", "wrong-source", "corrupt-hash", "read-failure", "cancel-child", "cancel-ready", "early-take", "budget"} {
		test.Run(scenario, func(test *testing.T) {
			stage, buffer := legacyCanonicalInitialCheckpointStage(test, `{"kind":"tool_result","seq":1,"output":{"nested":[1,true,null]}}`)
			store, objects := legacyNativeCheckpointStoreFixture(test)
			ref := legacyCanonicalInitialCheckpointRoot(test, stage, buffer, store)
			binding, sourceBytes := HashContent(buffer.body), len(buffer.body)
			expected := error(ErrIntegrity)
			switch scenario {
			case "wrong-binding":
				binding = HashContent([]byte("another-document"))
			case "wrong-source":
				sourceBytes++
			case "corrupt-hash":
				objects[ref.Hash][0] ^= 1
			case "read-failure":
				store.Read = func(context.Context, ContentHash, int) ([]byte, error) { return nil, ErrConflict }
				expected = ErrConflict
			case "cancel-child", "cancel-ready":
				expected = context.Canceled
			case "budget":
				expected = ErrDocumentResourceLimit
			}
			work, err := newLegacyCanonicalInitialRestore(context.Background(), ref, binding, sourceBytes, store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var failure error
			if scenario == "early-take" {
				_, failure = work.take(ctx)
			} else if scenario == "cancel-ready" {
				legacyDecoderCheckpointDrain(test, work, legacyDecoderCheckpointBudget)
				owner := work.encoding.nativeOwner
				cancel()
				_, failure = work.take(ctx)
				if owner.heap != (docRangeHeaderHeapEstimate{}) || !reflect.DeepEqual(owner.event, CIREvent{}) {
					test.Fatal("canceled handoff retained restored ownership")
				}
			} else {
				if scenario == "budget" {
					work.budget = 1
				}
				for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
					if scenario == "cancel-child" && work.child != nil && work.child.fields != nil {
						cancel()
					}
					ready, err := work.step(ctx)
					if ready {
						test.Fatal("failed restore exposed an encoding")
					}
					if err != nil {
						failure = err
						break
					}
				}
			}
			if !errors.Is(failure, expected) {
				test.Fatal("restore changed its failure cause", failure, expected)
			}
			if _, err := work.take(context.Background()); !errors.Is(err, failure) {
				test.Fatal("failed restore lost its sticky error", err)
			}
			legacyNativeAnyW78Released(test, work)
		})
	}
}

func TestLegacyCanonicalInitialRestoreRejectsNonSingleton(test *testing.T) {
	for _, scenario := range []string{"nil", "empty", "hidden-record", "two-records"} {
		test.Run(scenario, func(test *testing.T) {
			stage, buffer := legacyCanonicalInitialCheckpointStage(test, `{"kind":"turn","seq":1}`)
			owner := stage.pending[0].Encoding.nativeOwner
			list := &legacyNativeEventList{mutation: &legacyNormalizationMutation{}, paged: true, end: len(buffer.body)}
			if scenario != "nil" {
				list.values = &legacyNativeEventValues{}
			}
			if scenario == "hidden-record" || scenario == "two-records" {
				count := 1
				if scenario == "two-records" {
					count = 2
					list.values.count = 2
				}
				for index := 0; index < count; index++ {
					if err := list.values.records.append(legacyNativeEventValue{event: owner.event, heap: owner.heap}); err != nil {
						test.Fatal(err)
					}
					list.values.payload.include(owner.heap)
				}
			}
			store, _ := legacyNativeCheckpointStoreFixture(test)
			writer, err := newLegacyNativeEventValuesCheckpoint(context.Background(), list, HashContent(buffer.body), len(buffer.body), store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer writer.close()
			legacyDecoderCheckpointDrain(test, writer, legacyDecoderCheckpointBudget)
			ref, err := writer.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			restore, err := newLegacyCanonicalInitialRestore(context.Background(), ref, HashContent(buffer.body), len(buffer.body), store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			var failure error
			for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
				ready, err := restore.step(context.Background())
				if ready {
					test.Fatal("non-singleton native values became an initial encoding")
				}
				if err != nil {
					failure = err
					break
				}
			}
			if !errors.Is(failure, ErrIntegrity) {
				test.Fatal("non-singleton shape was not rejected", failure)
			}
			legacyNativeAnyW78Released(test, restore)
		})
	}
}

func TestLegacyCanonicalInitialCheckpointConstructorFences(test *testing.T) {
	stage, buffer := legacyCanonicalInitialCheckpointStage(test, `{"kind":"turn","seq":1}`)
	encoding := stage.pending[0].Encoding
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(buffer.body)
	if work, err := newLegacyCanonicalInitialCheckpoint(context.Background(), encoding, stage.mutation, binding, len(buffer.body), store, legacyDecoderCheckpointBudget); work != nil || !errors.Is(err, ErrConflict) {
		test.Fatal("unfrozen scope acquired an unaccounted capture grant", err)
	}
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if work, err := newLegacyCanonicalInitialCheckpoint(canceled, encoding, stage.mutation, binding, len(buffer.body), store, legacyDecoderCheckpointBudget); work != nil || !errors.Is(err, context.Canceled) {
		test.Fatal("constructor ignored cancellation", err)
	}
	if work, err := newLegacyCanonicalInitialCheckpoint(context.Background(), encoding, stage.mutation, binding, len(buffer.body), store, 1); work != nil || !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatal("constructor allocated outside its headroom", err)
	}
	if work, err := newLegacyCanonicalInitialCheckpoint(context.Background(), nil, stage.mutation, binding, len(buffer.body), store, legacyDecoderCheckpointBudget); work != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("constructor accepted a missing initial encoding", err)
	}
	if work, err := newLegacyCanonicalInitialCheckpoint(context.Background(), encoding, stage.mutation, binding, MaxFinalizedDocBytes+1, store, legacyDecoderCheckpointBudget); work != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("constructor changed the source-size limit", err)
	}
	heap := encoding.nativeOwner.heap
	encoding.nativeOwner.heap.bytes = math.MaxInt64
	work, overflow := newLegacyCanonicalInitialCheckpoint(context.Background(), encoding, stage.mutation, binding, len(buffer.body), store, legacyDecoderCheckpointBudget)
	encoding.nativeOwner.heap = heap
	if work != nil || !errors.Is(overflow, ErrIntegrity) {
		if work != nil {
			work.close()
		}
		test.Fatal("constructor allowed borrowed-budget arithmetic overflow", overflow)
	}
	if _, _, err := encoding.Next(context.Background()); err != nil {
		test.Fatal(err)
	}
	if work, err := newLegacyCanonicalInitialCheckpoint(context.Background(), encoding, stage.mutation, binding, len(buffer.body), store, legacyDecoderCheckpointBudget); work != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("constructor accepted canonical preparation after Next", err)
	}
	if grant.references.Load() != 1 {
		test.Fatal("constructor failure leaked shared capture references")
	}
}
