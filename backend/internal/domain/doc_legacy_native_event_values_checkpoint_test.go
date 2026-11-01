package domain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func legacyEventValuesCheckpointFixtures() []legacyDecoderCheckpointFixture {
	large := strings.Repeat("복구🙂", ConversationManifestChunkBytes/3)
	message := `{"kind":"message","seq":1,"role":"user","blocks":[{"type":"text","text":"` + large + `"},{"type":"text","text":"hidden-second"},{"type":"text","text":"hidden-third"}],"blocks":[{"type":"text","text":"visible"}]}`
	call := `{"kind":"tool_call","seq":2,"call_id":"call","tool_name":"test","input":{"hidden":["` + large + `",null,4]},"input":{"new":[true,"tail"]}}`
	result := `{"kind":"tool_result","seq":3,"call_id":"call","output":{"hidden":"` + large + `"},"output":"visible-scalar","is_error":false}`
	nested := `{"kind":"compaction","seq":4,"replacement_complete":true,"replacement":[` + message + `,` + call + `,` + result + `],"replacement":[` + message + `]}`
	root := `{"kind":"compaction","seq":0,"replacement_complete":true,"replacement":[` + nested + `,` + call + `,` + result + `]`
	prefix := `{"envelope":{"cir_version":"2","source_provider":"codex","fidelity":"full"},"events":[`
	fixtures := []legacyDecoderCheckpointFixture{
		{name: "active-with-nested-hidden-records-and-blocks", raw: prefix + `{"kind":"compaction","seq":0,"replacement_complete":true,"replacement":[` + nested + `,{"kind":"message","seq":9,"id":"` + large + `","role":"user","blocks":[]}]}]}`,
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
			}},
		{name: "inactive-cache-before-root-replacement-reuse", raw: prefix + root + `,"id":"` + large + `","replacement":[{"kind":"message","seq":8,"role":"user","blocks":[]},{"kind":"message","seq":10,"role":"assistant","blocks":[{"type":"text","text":"after-restore"}]}]}]}`,
			cut: func(decoder *LegacyNormalizationDecoder) bool {
				if decoder.native == nil || decoder.native.object.eventActive || decoder.native.object.scalar == nil {
					return false
				}
				for _, list := range decoder.native.object.events {
					if list != nil && list.phase == legacyNativeEventsDone && list.values != nil && list.values.count == 3 {
						return true
					}
				}
				return false
			}},
	}
	rejected := fixtures[1]
	rejected.name = "inactive-cache-before-null-replacement-rejection"
	rejected.raw = strings.Replace(rejected.raw, `{"kind":"message","seq":8,"role":"user","blocks":[]}`, `null`, 1)
	return append(fixtures, rejected)
}

func legacyEventValuesCheckpointRejectedContinuation(test *testing.T, decoder *LegacyNormalizationDecoder) []string {
	test.Helper()
	var trace []string
	for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
		step, err := decoder.Step(context.Background())
		if err != nil {
			return append(trace, "step-error:"+err.Error())
		}
		if step.Reset {
			trace = append(trace, "reset")
		}
		for _, event := range step.Events {
			body, err := legacyNormalizationTestBytes(context.Background(), event)
			if err != nil {
				return append(trace, "encode-error:"+err.Error())
			}
			trace = append(trace, fmt.Sprintf("event:%d:%s", event.Sequence, HashContent(body)))
		}
		if step.Complete {
			test.Fatal("null replacement child unexpectedly became a valid completed document")
		}
	}
	test.Fatal("deferred rejection did not terminate")
	return nil
}

func legacyEventValuesCheckpointList(test *testing.T, decoder *LegacyNormalizationDecoder) (*legacyNativeEventList, int) {
	test.Helper()
	if decoder.native != nil {
		for index, list := range decoder.native.object.events {
			if list != nil {
				return list, index
			}
		}
	}
	test.Fatal("actual decoder has no replacement list")
	return nil, 0
}

func legacyEventValuesCheckpointCompare(test *testing.T, expected, actual *legacyNativeEventValues) {
	test.Helper()
	type pair struct{ expected, actual *legacyNativeEventValues }
	pending := []pair{{expected, actual}}
	for len(pending) != 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current.expected == nil || current.actual == nil {
			if current.expected != nil || current.actual != nil {
				test.Fatal("nil replacement data changed")
			}
			continue
		}
		if current.expected.count != current.actual.count || current.expected.records.length() != current.actual.records.length() {
			test.Fatal("replacement visible/high-water shape changed")
		}
		var payload docRangeHeaderHeapEstimate
		for index := 0; index < current.expected.records.length(); index++ {
			left, right := current.expected.records.at(index), current.actual.records.at(index)
			if left == nil || right == nil || !reflect.DeepEqual(left.event, right.event) {
				test.Fatal("typed event fields or record order changed", index)
			}
			payload.include(right.heap)
			if right.heap.err != nil || right.excess < 0 || right.excess > right.heap.bytes {
				test.Fatal("restored event ledger is invalid", index)
			}
			if left.blocks == nil || right.blocks == nil {
				if left.blocks != nil || right.blocks != nil {
					test.Fatal("nil blocks changed", index)
				}
			} else {
				if left.blocks.count != right.blocks.count || left.blocks.records.length() != right.blocks.records.length() {
					test.Fatal("block high-water shape changed", index)
				}
				for block := 0; block < left.blocks.records.length(); block++ {
					if left.blocks.records.at(block).block != right.blocks.records.at(block).block {
						test.Fatal("block payload changed", index, block)
					}
				}
			}
			pending = append(pending, pair{left.replacement, right.replacement})
		}
		if payload != current.actual.payload || current.actual.heap().err != nil {
			test.Fatal("restored replacement payload ledger omitted retained records")
		}
	}
}

func TestLegacyNativeEventValuesCheckpointActualContinuation(test *testing.T) {
	for _, fixture := range legacyEventValuesCheckpointFixtures() {
		test.Run(fixture.name, func(test *testing.T) {
			decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
			baseline, _ := legacyDecoderCheckpointCut(test, fixture)
			list, field := legacyEventValuesCheckpointList(test, decoder)
			storage := legacyNativeAnyW78Store(test)
			binding := HashContent(buffer.body)
			capture, err := newLegacyNativeEventValuesCheckpoint(context.Background(), list, binding, decoder.source.Bytes, storage.io(), legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer capture.close()
			if _, err := decoder.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
				test.Fatal("data capture did not freeze its real decoder", err)
			}
			legacyNativeAnyW78Drain(test, capture, storage, legacyDecoderCheckpointBudget)
			root, err := capture.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			restore, err := newLegacyNativeEventValuesCheckpointRestore(context.Background(), root, binding, decoder.source.Bytes, storage.io(), legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			legacyNativeAnyW78Drain(test, restore, storage, legacyDecoderCheckpointBudget)
			result, err := restore.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			legacyEventValuesCheckpointCompare(test, list.values, result.Values)
			if err := decoder.mutation.enter(); err != nil {
				test.Fatal(err)
			}
			list.values, list.excess = result.Values, result.Excess
			list.refreshHeap()
			if !decoder.native.object.eventActive {
				decoder.native.object.heaps[field], list.borrowed = list.estimate, list.estimate
			}
			decoder.mutation.leave()
			continuation := legacyDecoderCheckpointContinue
			if strings.HasSuffix(fixture.name, "-rejection") {
				continuation = legacyEventValuesCheckpointRejectedContinuation
			}
			if got, want := continuation(test, decoder), continuation(test, baseline); !reflect.DeepEqual(got, want) {
				test.Fatal("real decoder changed after replacement data restoration", got, want)
			}
		})
	}
}

func TestLegacyNativeEventValuesCheckpointEmptyShapes(test *testing.T) {
	for _, scenario := range []string{"nil", "empty", "reserved-hidden"} {
		test.Run(scenario, func(test *testing.T) {
			list := &legacyNativeEventList{mutation: &legacyNormalizationMutation{}, paged: true}
			if scenario != "nil" {
				list.values = &legacyNativeEventValues{}
			}
			if scenario == "reserved-hidden" {
				if err := list.values.records.append(legacyNativeEventValue{}); err != nil {
					test.Fatal(err)
				}
			}
			storage := legacyNativeAnyW78Store(test)
			binding := HashContent([]byte("empty-values"))
			capture, err := newLegacyNativeEventValuesCheckpoint(context.Background(), list, binding, 1024, storage.io(), legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer capture.close()
			legacyNativeAnyW78Drain(test, capture, storage, legacyDecoderCheckpointBudget)
			root, err := capture.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			restore, err := newLegacyNativeEventValuesCheckpointRestore(context.Background(), root, binding, 1024, storage.io(), legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			legacyNativeAnyW78Drain(test, restore, storage, legacyDecoderCheckpointBudget)
			result, err := restore.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			legacyEventValuesCheckpointCompare(test, list.values, result.Values)
		})
	}
}
