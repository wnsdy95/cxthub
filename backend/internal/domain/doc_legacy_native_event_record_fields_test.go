package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func legacyNativeEventRecordFieldsCut(test *testing.T) (*LegacyNormalizationDecoder, *legacyNativeEventList) {
	test.Helper()
	large := strings.Repeat("replacement-\ucd5c\uc2e0", ConversationManifestChunkBytes/4)
	fixture := legacyDecoderCheckpointFixture{name: "completed-child-before-active-replacement", raw: `{"envelope":{"cir_version":"2","source_provider":"codex","fidelity":"full"},"events":[{"kind":"compaction","seq":0,"replacement":[{"kind":"not-a-valid-kind","id":"retained","seq":9007199254740993,"role":"assistant","input":{"nested":[null,false,3.5,"` + large + `"]},"output":{"visible":"` + large + `"},"is_error":false,"cross_replayable":true,"locked":{"provider":"codex","scheme":"opaque","blob":"` + large + `"},"provider_metadata":{"turn_id":"kept","create_time":9007199254740993},"agent_message":true,"agent_author":"author","agent_recipient":"recipient","compact_summary":true,"replacement_complete":true,"tool_name":"must-not-disappear","call_id":"call","provider_tool_name":"native","status":"done","redacted_summary":"kept"},{"kind":"message","seq":2,"id":"` + large + `","role":"user","blocks":[]}]}]}`,
		cut: func(decoder *LegacyNormalizationDecoder) bool {
			if decoder.native == nil || !decoder.native.object.eventActive {
				return false
			}
			for _, list := range decoder.native.object.events {
				if list != nil && list.values != nil && list.values.count == 1 && list.phase == legacyNativeEventsApply && list.object != nil && list.object.scalar != nil {
					return true
				}
			}
			return false
		}}
	decoder, _ := legacyDecoderCheckpointCut(test, fixture)
	for _, list := range decoder.native.object.events {
		if list != nil && list.values != nil && list.values.count == 1 {
			return decoder, list
		}
	}
	test.Fatal("actual replacement did not retain its completed child")
	return nil, nil
}

func legacyNativeEventRecordFieldsRoundTrip(test *testing.T, list *legacyNativeEventList) legacyNativeEventRecordFieldsRestored {
	test.Helper()
	storage := legacyNativeAnyW78Store(test)
	binding := HashContent([]byte("actual-replacement-record"))
	scope := list.mutation
	work, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, binding, storage.io(), legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	if err := scope.enter(); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("record capture did not freeze its actual parser", err)
	}
	legacyNativeAnyW78Drain(test, work, storage, legacyDecoderCheckpointBudget)
	root, err := work.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if err := scope.enter(); err != nil {
		test.Fatal("successful capture did not release its barrier", err)
	}
	scope.leave()
	restore, err := newLegacyNativeEventRecordFieldsRestore(context.Background(), root, binding, storage.io(), legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	legacyNativeAnyW78Drain(test, restore, storage, legacyDecoderCheckpointBudget)
	result, err := restore.take(context.Background())
	if err != nil || result.Heap.err != nil || result.Heap.bytes < 0 {
		test.Fatal("restored fields lost their retained payload", result.Heap, err)
	}
	return result
}

func TestLegacyNativeEventRecordFieldsActualReplacement(test *testing.T) {
	decoder, list := legacyNativeEventRecordFieldsCut(test)
	record := list.values.at(0)
	if _, err := eventJSONObject(record.event); err == nil {
		test.Fatal("fixture must remain an invalid typed child until canonical validation", err)
	}
	result := legacyNativeEventRecordFieldsRoundTrip(test, list)
	if !reflect.DeepEqual(record.event, result.Event) {
		test.Fatal("completed typed child fields changed before canonical validation")
	}
	if result.Heap.bytes > record.heap.bytes {
		test.Fatal("restored fields exceeded their original completed-record ledger", result.Heap, record.heap)
	}
	if _, err := eventJSONObject(result.Event); err == nil {
		test.Fatal("checkpoint made an invalid child valid", err)
	}
	var rejected bool
	for attempt := 0; attempt < legacyDecoderCheckpointSteps && !rejected; attempt++ {
		step, err := decoder.Step(context.Background())
		if err != nil || step.Complete {
			test.Fatal("actual decoder left its deferred canonical rejection path", err)
		}
		for _, event := range step.Events {
			_, err := legacyNormalizationTestBytes(context.Background(), event)
			if err == nil {
				test.Fatal("invalid replacement was emitted successfully")
			}
			rejected = true
		}
	}
	if !rejected {
		test.Fatal("actual decoder did not reach deferred canonical rejection")
	}
}

func TestLegacyNativeEventRecordFieldsExactValues(test *testing.T) {
	for _, scenario := range []string{"all-zero", "alias-and-invalid-number", "typed-nil-output", "empty-input"} {
		test.Run(scenario, func(test *testing.T) {
			record := &legacyNativeEventValue{}
			switch scenario {
			case "alias-and-invalid-number":
				boolean := true
				number := json.Number("not-a-number-yet")
				shared := map[string]any{"preserved": []any{"value", nil}}
				record.event = CIREvent{Kind: "unknown", Seq: -9007199254740993, Input: shared, Output: shared, IsError: &boolean, CrossReplayable: &boolean,
					ProviderMetadata: &ProviderMetadata{CreateTime: &number}, fieldPresence: ^eventFieldPresence(0)}
			case "typed-nil-output":
				record.event.Output = []any(nil)
			case "empty-input":
				record.event.Input = map[string]any{}
			}
			record.heap.bytes = 1 << 16
			list := legacyNativeEventRecordFieldsTestList(test, *record)
			result := legacyNativeEventRecordFieldsRoundTrip(test, list)
			if !reflect.DeepEqual(record.event, result.Event) {
				test.Fatalf("typed fields changed: got %#v want %#v", result.Event, record.event)
			}
			if scenario == "alias-and-invalid-number" {
				if result.Event.IsError != result.Event.CrossReplayable || reflect.ValueOf(result.Event.Input).Pointer() != reflect.ValueOf(result.Event.Output).Pointer() {
					test.Fatal("field aliases split during restore")
				}
			}
		})
	}
}

func TestLegacyNativeEventRecordFieldsFailureFences(test *testing.T) {
	for _, scenario := range []string{"budget", "cancel", "put", "source-stop", "binding", "read", "corrupt", "schema", "child-not-paged"} {
		test.Run(scenario, func(test *testing.T) {
			storage := legacyNativeAnyW78Store(test)
			record := &legacyNativeEventValue{event: CIREvent{ID: "before"}}
			list := legacyNativeEventRecordFieldsTestList(test, *record)
			scope := list.mutation
			binding := HashContent([]byte("record-fences"))
			budget := legacyDecoderCheckpointBudget
			if scenario == "budget" {
				budget = 1
			}
			if scenario == "child-not-paged" {
				list.values.at(0).event.Replacement = []CIREvent{}
			}
			work, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, binding, storage.io(), budget)
			if scenario == "budget" || scenario == "child-not-paged" {
				if err == nil {
					work.close()
					test.Fatal("unsupported ownership or exhausted budget accepted")
				}
				if err := scope.enter(); err != nil {
					test.Fatal("construction failure leaked capture", err)
				}
				scope.leave()
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			if scenario == "cancel" || scenario == "put" || scenario == "source-stop" {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if scenario == "cancel" {
					cancel()
				} else if scenario == "put" {
					storage.failPut = ErrConflict
				} else {
					scope.stop()
				}
				var failure error
				for attempt := 0; attempt < legacyNativeAnyW78StepLimit; attempt++ {
					ready, err := work.step(ctx)
					if ready {
						test.Fatal("failed capture published a root")
					}
					if err != nil {
						failure = err
						break
					}
				}
				if failure == nil {
					test.Fatal("failed capture never terminated")
				}
				if scope.activity.Load() != 0 || scope.capture.Load() != nil {
					test.Fatal("failed capture retained its grant")
				}
				if scenario != "source-stop" {
					if err := scope.enter(); err != nil {
						test.Fatal("failed capture retained mutation exclusion", err)
					}
					scope.leave()
				}
				return
			}
			legacyNativeAnyW78Drain(test, work, storage, budget)
			root, err := work.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			if scenario == "binding" {
				binding = HashContent([]byte("another-record"))
			}
			if scenario == "read" {
				storage.failRead = ErrConflict
			}
			if scenario == "corrupt" || scenario == "schema" {
				var state legacyNativeEventRecordFieldsState
				if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), root, &state); err != nil {
					test.Fatal(err)
				}
				if scenario == "corrupt" {
					state.BooleanAlias = true
				} else {
					state.Schema = HashContent([]byte("wrong-model"))
				}
				root, err = legacyNativeCheckpointWriteJSON(context.Background(), storage.io(), state)
				if err != nil {
					test.Fatal(err)
				}
			}
			restore, err := newLegacyNativeEventRecordFieldsRestore(context.Background(), root, binding, storage.io(), budget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			if ready, err := restore.step(context.Background()); ready || err == nil {
				test.Fatal("untrusted field state was restored", ready, err)
			}
			if _, err := restore.take(context.Background()); err == nil {
				test.Fatal("failed restore yielded fields")
			}
		})
	}
}

func legacyNativeEventRecordFieldsTestList(test *testing.T, record legacyNativeEventValue) *legacyNativeEventList {
	test.Helper()
	values := &legacyNativeEventValues{count: 1, payload: record.heap}
	if err := values.records.append(record); err != nil {
		test.Fatal(err)
	}
	return &legacyNativeEventList{mutation: &legacyNormalizationMutation{}, paged: true, values: values, count: 1}
}

func TestLegacyNativeEventRecordFieldsActualOwnerAndDetachedPage(test *testing.T) {
	decoder, list := legacyNativeEventRecordFieldsCut(test)
	storage := legacyNativeAnyW78Store(test)
	work, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, HashContent([]byte("owner")), storage.io(), legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	if _, err := decoder.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("record capture did not block its actual decoder", err)
	}
	unrelated := &legacyNormalizationMutation{}
	if err := unrelated.enter(); err != nil {
		test.Fatal("capture locked an unrelated owner", err)
	}
	unrelated.leave()
	if reflect.TypeOf(work.record).Kind() == reflect.Pointer {
		test.Fatal("field capture retained an interior record pointer")
	}
	decoder.mutation.stop()
	if ready, err := work.step(context.Background()); ready || !errors.Is(err, ErrConflict) {
		test.Fatal("stopped actual source still issued a field root", ready, err)
	}
}

func TestLegacyNativeEventRecordFieldsRejectUncommittedSlot(test *testing.T) {
	decoder, list := legacyNativeEventRecordFieldsCut(test)
	if list.values.records.length() <= list.values.count {
		test.Fatal("actual active child did not reserve its next slot")
	}
	storage := legacyNativeAnyW78Store(test)
	work, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, list.values.count, HashContent([]byte("uncommitted")), storage.io(), legacyDecoderCheckpointBudget)
	if err == nil {
		work.close()
		test.Fatal("reserved but uncommitted slot was captured as a completed record")
	}
	legacyNormalizationCaptureRQ87Idle(test, decoder.mutation)
	if storage.writes != 0 {
		test.Fatal("invalid record selection wrote private objects")
	}
}

func TestLegacyNativeEventRecordFieldsRestoredBacking(test *testing.T) {
	list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: CIREvent{ID: strings.Repeat("x", LegacyCanonicalEncodingBytes+1)}, heap: docRangeHeaderHeapEstimate{bytes: 2 << 20}})
	storage := legacyNativeAnyW78Store(test)
	binding := HashContent([]byte("backing"))
	write, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, binding, storage.io(), legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer write.close()
	legacyNativeAnyW78Drain(test, write, storage, legacyDecoderCheckpointBudget)
	root, err := write.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	restore, err := newLegacyNativeEventRecordFieldsRestore(context.Background(), root, binding, storage.io(), legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	var capacity int
	for attempt := 0; attempt < legacyNativeAnyW78StepLimit; attempt++ {
		if restore.graph != nil && restore.graph.ready {
			capacity = restore.graph.values[1].capacity
			break
		}
		if ready, err := restore.step(context.Background()); err != nil || ready {
			test.Fatal("restore bypassed its observed field backing", ready, err)
		}
		if restore.graph != nil {
			if _, err := restore.graph.step(context.Background()); err != nil {
				test.Fatal(err)
			}
		}
	}
	if capacity < len(list.values.at(0).event.ID) {
		test.Fatal("field backing was not observed")
	}
	legacyNativeAnyW78Drain(test, restore, storage, legacyDecoderCheckpointBudget)
	result, err := restore.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(int64(capacity), 1)
	if result.Heap != expected || result.Excess != 2*int64(capacity-len(result.Event.ID)) {
		test.Fatal("restored field backing capacity disappeared from its ledger", capacity, result.Heap, result.Excess, expected)
	}
}
