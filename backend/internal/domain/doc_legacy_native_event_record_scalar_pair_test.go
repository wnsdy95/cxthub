package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeEventScalarPairParser struct {
	*legacyNativeEventValuesCheckpoint
}

func (work legacyNativeEventScalarPairParser) step(ctx context.Context) (bool, error) {
	return work.parserStep(ctx)
}

type legacyNativeEventScalarPairObject struct {
	Hash ContentHash
	Body []byte
}

type legacyNativeEventScalarPairCapture struct {
	root       legacyNativeCheckpointRef
	objects    []legacyNativeEventScalarPairObject
	steps      int
	pairWrites int
	bytes      int
}

func legacyNativeEventScalarPairCaptureList(test *testing.T, list *legacyNativeEventList, binding ContentHash, sourceBytes int, parser bool) legacyNativeEventScalarPairCapture {
	test.Helper()
	storage := legacyNativeGoGraphSetW85Storage(test)
	var work *legacyNativeEventValuesCheckpoint
	var result legacyNativeEventScalarPairCapture
	put := storage.io.Put
	storage.io.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		result.objects = append(result.objects, legacyNativeEventScalarPairObject{Hash: hash, Body: bytes.Clone(body)})
		result.bytes += len(body)
		if work.scalarPairHeap != 0 {
			result.pairWrites++
			retained, err := work.retainedBytes()
			if err != nil || work.scalarPairHeap != legacyNativeEventRecordScalarPairWorkspace().bytes || retained > work.budget || retained < work.scalarPairHeap {
				test.Fatal("scalar pair omitted its bounded live workspace", retained, work.scalarPairHeap, err)
			}
			if err := list.mutation.enter(); !errors.Is(err, ErrLegacyNormalizationBusy) {
				test.Fatal("scalar pair released its source freeze during a put", err)
			}
		}
		return put(ctx, hash, body)
	}
	var err error
	work, err = newLegacyNativeEventValuesCheckpoint(context.Background(), list, binding, sourceBytes, storage.io, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	var writer legacyNativeGoGraphSetW85Work[legacyNativeCheckpointRef] = legacyEventValuesAdministrativeOriginal{work}
	if parser {
		writer = legacyNativeEventScalarPairParser{work}
	}
	for result.steps < legacyNativeAnyW78StepLimit {
		writes, pairWrites := storage.writes, result.pairWrites
		ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, writer, storage)
		result.steps++
		if err != nil || work.scalarPairHeap != 0 {
			test.Fatal("scalar pair failed or retained ephemeral scratch", err, work.scalarPairHeap)
		}
		if result.pairWrites != pairWrites && (result.pairWrites-pairWrites != 2 || storage.writes-writes != 2 ||
			work.current == nil || work.current.phase != 3 || work.fields != nil) {
			test.Fatal("scalar pair crossed into another record or omitted a fresh put")
		}
		retained, err := work.retainedBytes()
		if err != nil || retained <= 0 || retained > work.budget {
			test.Fatal("scalar capture escaped bounded ownership", retained, err)
		}
		if !ready {
			continue
		}
		result.root, err = work.take(context.Background())
		if err != nil || !result.root.valid() {
			test.Fatal("scalar capture did not transfer its root", err)
		}
		legacyNativeGoGraphSetW85Released[legacyNativeCheckpointRef](test, writer)
		restore, err := newLegacyNativeEventValuesCheckpointRestore(context.Background(), result.root, binding, sourceBytes, storage.io, legacyDecoderCheckpointBudget)
		if err != nil {
			test.Fatal(err)
		}
		defer restore.close()
		legacyNativeGoGraphSetW85Ready[legacyNativeEventValuesRestored](test, restore, storage, legacyDecoderCheckpointBudget)
		restored, err := restore.take(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		legacyEventValuesCheckpointCompare(test, list.values, restored.Values)
		for position := 0; position < list.values.records.length(); position++ {
			before, after := list.values.records.at(position).event, restored.Values.records.at(position).event
			if before.IsError != nil && before.CrossReplayable != nil && (before.IsError == before.CrossReplayable) != (after.IsError == after.CrossReplayable) {
				test.Fatal("scalar pair changed boolean pointer aliasing")
			}
		}
		legacyNormalizationCaptureRQ87Idle(test, list.mutation)
		return result
	}
	test.Fatal("scalar capture exceeded the existing fixture schedule")
	return result
}

func TestLegacyNativeEventRecordScalarPairRawWire(test *testing.T) {
	child := `{"kind":"message","role":"assistant","blocks":[{"type":"text","text":"` + strings.Repeat("x", 64) + `"}]}`
	event := `{"kind":"compaction","replacement":[` + strings.Repeat(child+",", 255) + child + `],"replacement_complete":true}`
	stage, buffer := legacyCanonicalInitialCheckpointStage(test, event)
	list := &legacyNativeEventList{mutation: stage.mutation, paged: true, values: stage.pending[0].Encoding.nativeOwner.replacement}
	if list.values.count != 256 || list.values.records.length() != 256 {
		test.Fatal("scalar pair oracle lost its original raw record shape")
	}
	binding := HashContent(buffer.body)
	before := legacyNativeEventScalarPairCaptureList(test, list, binding, len(buffer.body), false)
	after := legacyNativeEventScalarPairCaptureList(test, list, binding, len(buffer.body), true)
	if before.root != after.root || !reflect.DeepEqual(before.objects, after.objects) || before.bytes != after.bytes {
		test.Fatal("scalar pair changed ordered object bytes, hashes, or the root")
	}
	if before.steps != 3844 || after.steps >= 802 || len(after.objects) != 549 || after.bytes != 1038470 || after.pairWrites != 512 {
		test.Fatal("scalar pair lost fresh per-record puts or the bounded step improvement", before.steps, after.steps, len(after.objects), after.bytes, after.pairWrites)
	}
	test.Logf("original_steps=%d parser_steps=%d puts=%d pair_puts=%d bytes=%d", before.steps, after.steps, len(after.objects), after.pairWrites, after.bytes)
}

func TestLegacyNativeEventRecordScalarPairExactFields(test *testing.T) {
	for _, scenario := range []string{"zero", "typed-nil-output", "all-short-fields", "aliased-booleans", "distinct-booleans"} {
		test.Run(scenario, func(test *testing.T) {
			event := CIREvent{}
			boolean, otherBoolean := true, true
			switch scenario {
			case "typed-nil-output":
				event.Output = map[string]any(nil)
			case "all-short-fields":
				text := strings.Repeat("\xff", 64)
				event = CIREvent{Kind: EventKind(text), ID: text, TS: text, Role: Role(text), Seq: -9007199254740993,
					AgentAuthor: text, AgentRecipient: text, CallID: text, ToolName: text, ProviderToolName: text,
					Status: text, RedactedSummary: text, fieldPresence: ^eventFieldPresence(0), CompactSummary: true,
					AgentMessage: true, ReplacementComplete: true}
			case "aliased-booleans":
				event.IsError, event.CrossReplayable = &boolean, &boolean
			case "distinct-booleans":
				event.IsError, event.CrossReplayable = &boolean, &otherBoolean
			}
			list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: event, heap: docRangeHeaderHeapEstimate{bytes: 1 << 16}})
			binding := HashContent([]byte(scenario))
			before := legacyNativeEventScalarPairCaptureList(test, list, binding, 4096, false)
			after := legacyNativeEventScalarPairCaptureList(test, list, binding, 4096, true)
			if before.root != after.root || !reflect.DeepEqual(before.objects, after.objects) || after.pairWrites != 2 {
				test.Fatal("scalar fields changed their exact slow-writer wire")
			}
			var graph legacyNativeCheckpointGoGraphSetState
			if err := json.Unmarshal(after.objects[0].Body, &graph); err != nil || len(graph.Roots) != legacyNativeEventRecordFieldRoots {
				test.Fatal("scalar pair did not retain the existing graph descriptor", err)
			}
			bounds := legacyNativeValueCheckpointBounds{PayloadBytes: legacyNativeValueCheckpointPayloadLimit(), Nodes: legacyNativeCheckpointNodes}
			input, err := decodeLegacyNativeValueCheckpoint(context.Background(), graph.Roots[16].Wire, bounds)
			if err != nil || input.Kind != legacyNativeCheckpointObject || !input.Nil {
				test.Fatal("nil CIREvent.Input lost its typed-nil map root", input, err)
			}
			output, err := decodeLegacyNativeValueCheckpoint(context.Background(), graph.Roots[17].Wire, bounds)
			if err != nil || scenario == "typed-nil-output" && (output.Kind != legacyNativeCheckpointObject || !output.Nil) ||
				scenario != "typed-nil-output" && output.Kind != legacyNativeCheckpointNull {
				test.Fatal("scalar output lost its nil dynamic type", output, err)
			}
		})
	}
}

func TestLegacyNativeEventRecordScalarPairFallback(test *testing.T) {
	number := json.Number("1e-7")
	for _, fixture := range []struct {
		name  string
		event CIREvent
	}{
		{name: "long-text", event: CIREvent{ID: strings.Repeat("x", 65)}},
		{name: "empty-input", event: CIREvent{Input: map[string]any{}}},
		{name: "input-graph", event: CIREvent{Input: map[string]any{"nested": []any{true, "value"}}}},
		{name: "output-graph", event: CIREvent{Output: map[string]any{}}},
		{name: "output-string", event: CIREvent{Output: "value"}},
		{name: "typed-nil-array", event: CIREvent{Output: []any(nil)}},
		{name: "metadata", event: CIREvent{ProviderMetadata: &ProviderMetadata{TurnID: "turn"}}},
		{name: "create-time", event: CIREvent{ProviderMetadata: &ProviderMetadata{CreateTime: &number}}},
		{name: "locked", event: CIREvent{Locked: &LockedBlob{Scheme: "opaque"}}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: fixture.event, heap: docRangeHeaderHeapEstimate{bytes: 1 << 16}})
			binding := HashContent([]byte(fixture.name))
			before := legacyNativeEventScalarPairCaptureList(test, list, binding, 4096, false)
			after := legacyNativeEventScalarPairCaptureList(test, list, binding, 4096, true)
			if before.root != after.root || !reflect.DeepEqual(before.objects, after.objects) || after.pairWrites != 0 {
				test.Fatal("complex record left its original writer")
			}
		})
	}
}

func TestLegacyNativeEventRecordScalarPairCallbackFences(test *testing.T) {
	for _, scenario := range []string{"put-error", "cancel", "source-stop", "source-change", "alias-change", "budget"} {
		for _, boundary := range []int{1, 2} {
			test.Run(fmt.Sprintf("%s/put-%d", scenario, boundary), func(test *testing.T) {
				boolean := true
				list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: CIREvent{ID: "before", IsError: &boolean, CrossReplayable: &boolean}})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var work *legacyNativeEventValuesCheckpoint
				attempts := 0
				wanted := error(ErrConflict)
				storage := legacyNativeCheckpointIO{Put: func(current context.Context, hash ContentHash, body []byte) error {
					attempts++
					retained, err := work.retainedBytes()
					if current != ctx || len(body) > legacyNativeEventRecordScalarPairBytes || HashContent(body) != hash ||
						err != nil || work.scalarPairHeap == 0 || retained > work.budget || !work.capture.valid(list.mutation) {
						test.Fatal("pair callback lost current context, bytes, budget, or freeze", err)
					}
					if attempts != boundary {
						return nil
					}
					switch scenario {
					case "put-error":
						wanted = ErrPreparedDocRangeObject
						return wanted
					case "cancel":
						wanted = context.Canceled
						cancel()
					case "source-stop":
						list.mutation.stop()
					case "source-change":
						list.values.records.at(0).event.ID = "changed"
					case "alias-change":
						boolean = false
					case "budget":
						wanted = ErrDocumentResourceLimit
						work.budget = retained - 1
					}
					return nil
				}}
				var err error
				work, err = newLegacyNativeEventValuesCheckpoint(ctx, list, HashContent([]byte("pair-fences")), 4096, storage, legacyDecoderCheckpointBudget)
				if err != nil {
					test.Fatal(err)
				}
				defer work.close()
				ready, err := work.parserStep(ctx)
				if ready || attempts != boundary || !errors.Is(err, wanted) {
					test.Fatal("pair crossed a failed callback boundary", ready, attempts, err, wanted)
				}
				if retained, err := work.retainedBytes(); err != nil || retained != 0 || work.scalarPairHeap != 0 ||
					list.mutation.activity.Load() != 0 || list.mutation.capture.Load() != nil {
					test.Fatal("failed pair retained scratch or capture ownership", retained, err)
				}
				if ready, err := work.parserStep(ctx); ready || !errors.Is(err, wanted) || attempts != boundary {
					test.Fatal("failed pair retried a put or lost its sticky error", ready, err)
				}
				if root, err := work.take(ctx); root != (legacyNativeCheckpointRef{}) || !errors.Is(err, wanted) {
					test.Fatal("failed pair exposed a root", root, err)
				}
			})
		}
	}
}

func TestLegacyNativeEventRecordScalarPairAdmission(test *testing.T) {
	for _, scenario := range []string{"budget", "cancel"} {
		test.Run(scenario, func(test *testing.T) {
			list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{})
			attempts := 0
			storage := legacyNativeCheckpointIO{Put: func(context.Context, ContentHash, []byte) error {
				attempts++
				return nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			work, err := newLegacyNativeEventValuesCheckpoint(ctx, list, HashContent([]byte("pair-admission")), 4096, storage, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			wanted := error(context.Canceled)
			if scenario == "cancel" {
				cancel()
			} else {
				retained, err := work.retainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				work.budget = retained + legacyNativeEventRecordScalarPairWorkspace().bytes - 1
				wanted = ErrDocumentResourceLimit
			}
			if ready, err := work.parserStep(ctx); ready || !errors.Is(err, wanted) || attempts != 0 {
				test.Fatal("unadmitted pair performed IO", ready, err, attempts)
			}
			legacyNormalizationCaptureRQ87Idle(test, list.mutation)
		})
	}
}

func TestLegacyNativeEventRecordScalarPairCanonicalScope(test *testing.T) {
	for _, event := range []string{
		`{"kind":"message","role":"assistant","blocks":[{"type":"text","text":"value"}]}`,
		`{"kind":"tool_result","output":{"value":true},"provider_metadata":{"create_time":1e-7}}`,
	} {
		stage, buffer := legacyCanonicalInitialCheckpointStage(test, event)
		grant, err := stage.mutation.beginCapture()
		if err != nil {
			test.Fatal(err)
		}
		storage := legacyNativeGoGraphSetW85Storage(test)
		work, err := newLegacyCanonicalInitialCheckpoint(context.Background(), stage.pending[0].Encoding, stage.mutation,
			HashContent(buffer.body), len(buffer.body), storage.io, legacyDecoderCheckpointBudget)
		if err != nil {
			grant.release()
			test.Fatal(err)
		}
		graphHandoff, recordHandoff, ready := false, false, false
		for attempts := 0; attempts < legacyNativeGoGraphSetW85Steps && !ready; attempts++ {
			ready, err = legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, work, storage)
			if err != nil || work.child.scalarPairHeap != 0 {
				work.close()
				grant.release()
				test.Fatal("canonical source entered parser-only scalar capture", err)
			}
			graphHandoff = graphHandoff || work.sourceGraph() != nil
			recordHandoff = recordHandoff || work.sourceRecord() != nil
		}
		work.close()
		grant.release()
		if !ready || !graphHandoff || !recordHandoff {
			test.Fatal("canonical source lost its graph or record handoff", ready, graphHandoff, recordHandoff)
		}
	}
}
