package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type legacyEventValuesAdministrativeOriginal struct {
	*legacyNativeEventValuesCheckpoint
}

func (original legacyEventValuesAdministrativeOriginal) step(ctx context.Context) (bool, error) {
	work := original.legacyNativeEventValuesCheckpoint
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
	if err := work.admit(4 * LegacyCanonicalEncodingBytes); err != nil {
		return work.fail(err)
	}
	if err := work.advance(ctx); err != nil {
		return work.fail(err)
	}
	if err := work.checkSource(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return work.ready, nil
}

type legacyEventValuesAdministrativeTrace struct {
	state    string
	stop     bool
	reads    int
	writes   int
	bytes    int
	retained int64
}

func legacyEventValuesAdministrativeState(test *testing.T, work *legacyNativeEventValuesCheckpoint) string {
	test.Helper()
	var state strings.Builder
	rows, err := json.Marshal(work.rows.values)
	if err != nil {
		test.Fatal(err)
	}
	fmt.Fprintf(&state, "%t/%t/%#v/%s/%#v", work.ready, work.finished, work.root, rows, work.rows.tree)
	for frame := work.current; frame != nil; frame = frame.parent {
		fmt.Fprintf(&state, "/frame=%d/%d/%d/%d/%#v/%#v", frame.phase, frame.index, frame.blockIndex, frame.recordRow, frame.fields, frame.payload)
	}
	if fields := work.fields; fields != nil {
		fmt.Fprintf(&state, "/fields=%t/%#v/%#v", fields.ready, fields.root, fields.state)
		if graph := fields.graph; graph != nil {
			body, err := json.Marshal(graph.state)
			if err != nil {
				test.Fatal(err)
			}
			fmt.Fprintf(&state, "/graph=%d/%d/%t/%#v/%s", graph.phase, graph.position, graph.ready, graph.ref, body)
			if layout := graph.layout; layout != nil {
				fmt.Fprintf(&state, "/layout=%d/%d/%d/%d/%d/%d/%t", layout.phase, layout.rootIndex, layout.position, layout.nodes, layout.visits, layout.depth, layout.ready)
				for frame := layout.frame; frame != nil; frame = frame.parent {
					fmt.Fprintf(&state, "/layout-frame=%d", frame.position)
				}
			}
		}
	}
	return state.String()
}

func legacyEventValuesAdministrativeCapture(test *testing.T, list *legacyNativeEventList, binding ContentHash, sourceBytes int, original bool, expected []legacyEventValuesAdministrativeTrace) (legacyNativeCheckpointRef, *legacyNativeGoGraphSetW85Store, []legacyEventValuesAdministrativeTrace, []legacyNativeCheckpointRef) {
	test.Helper()
	storage := legacyNativeGoGraphSetW85Storage(test)
	var calls []legacyNativeCheckpointRef
	put := storage.io.Put
	storage.io.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		calls = append(calls, legacyNativeCheckpointRef{Hash: hash, Bytes: len(body)})
		return put(ctx, hash, body)
	}
	work, err := newLegacyNativeEventValuesCheckpoint(context.Background(), list, binding, sourceBytes, storage.io, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	var trace []legacyEventValuesAdministrativeTrace
	consumed, maximum, cascades, handoffs := 0, 0, 0, 0
	for range legacyNativeAnyW78StepLimit {
		reads, writes := storage.reads, storage.writes
		var ready bool
		if original {
			ready, err = legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, legacyEventValuesAdministrativeOriginal{work}, storage)
		} else {
			ready, err = legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, work, storage)
		}
		if err != nil {
			test.Fatal(err)
		}
		retained, err := work.retainedBytes()
		if err != nil || retained <= 0 || retained > work.budget {
			test.Fatal("coalesced capture escaped aggregate heap admission", retained, err)
		}
		handoff := work.current != nil && work.current.phase == 7
		if handoff {
			handoffs++
		}
		if storage.writes-writes > 1 {
			cascades++
		}
		entry := legacyEventValuesAdministrativeTrace{state: legacyEventValuesAdministrativeState(test, work), stop: handoff || storage.reads != reads || storage.writes != writes,
			reads: storage.reads, writes: storage.writes, bytes: storage.transferred, retained: retained}
		trace = append(trace, entry)
		if expected != nil {
			start := consumed
			for consumed < len(expected) && consumed-start < 8 {
				previous := expected[consumed]
				consumed++
				if entry.state == previous.state {
					if entry.reads != previous.reads || entry.writes != previous.writes || entry.bytes != previous.bytes || entry.retained != previous.retained {
						test.Fatal("coalescing changed fresh IO or retained ownership at an original boundary")
					}
					break
				}
				if previous.stop {
					test.Fatal("outer step crossed an IO attempt or sourceRecord handoff", consumed)
				}
			}
			if consumed == start || entry.state != expected[consumed-1].state {
				test.Fatal("outer step exceeded eight original transitions", consumed-start)
			}
			maximum = max(maximum, consumed-start)
		}
		if ready {
			if expected != nil && consumed != len(expected) {
				test.Fatal("coalescing omitted original transitions")
			}
			root, err := work.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			legacyNativeGoGraphSetW85Released[legacyNativeCheckpointRef](test, work)
			test.Logf("steps=%d maximum_original_transitions=%d handoffs=%d cascade_steps=%d puts=%d bytes=%d", len(trace), maximum, handoffs, cascades, storage.writes, storage.transferred)
			if expected != nil && (cascades == 0 || handoffs != list.values.records.length()) {
				test.Fatal("raw fixture omitted a full-page tree cascade or complete-record handoff", cascades, handoffs)
			}
			return root, storage, trace, calls
		}
	}
	test.Fatal("capture exceeded the existing finite fixture schedule")
	return legacyNativeCheckpointRef{}, storage, trace, calls
}

func TestLegacyNativeEventValuesAdministrativeRawOracle(test *testing.T) {
	child := `{"kind":"message","role":"assistant","blocks":[{"type":"text","text":"` + strings.Repeat("x", 64) + `"}]}`
	event := `{"kind":"compaction","replacement":[` + strings.Repeat(child+",", 255) + child + `],"replacement_complete":true}`
	stage, buffer := legacyCanonicalInitialCheckpointStage(test, event)
	list := &legacyNativeEventList{mutation: stage.mutation, paged: true, values: stage.pending[0].Encoding.nativeOwner.replacement}
	if list.values.count != 256 || list.values.records.length() != 256 {
		test.Fatal("raw production-shape fixture lost its completed records")
	}
	binding := HashContent(buffer.body)
	before, oldStore, oldTrace, oldCalls := legacyEventValuesAdministrativeCapture(test, list, binding, len(buffer.body), true, nil)
	after, newStore, newTrace, newCalls := legacyEventValuesAdministrativeCapture(test, list, binding, len(buffer.body), false, oldTrace)
	if before != after || !reflect.DeepEqual(oldStore.objects, newStore.objects) || !reflect.DeepEqual(oldCalls, newCalls) {
		test.Fatal("administrative coalescing changed exact wire, root hash, or ordered fresh CAS proofs")
	}
	if len(oldTrace) != 15*256+4 || len(newTrace) != 802 {
		test.Fatal("raw record step reduction changed", len(oldTrace), len(newTrace))
	}
	legacyNormalizationCaptureRQ87Idle(test, stage.mutation)
}

func TestLegacyNativeEventValuesAdministrativeNestedWire(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyEventValuesCheckpointFixtures()[0])
	list, _ := legacyEventValuesCheckpointList(test, decoder)
	binding := HashContent(buffer.body)
	before, oldStore, oldTrace, oldCalls := legacyEventValuesAdministrativeCapture(test, list, binding, decoder.source.Bytes, true, nil)
	after, newStore, newTrace, newCalls := legacyEventValuesAdministrativeCapture(test, list, binding, decoder.source.Bytes, false, nil)
	if before != after || !reflect.DeepEqual(oldStore.objects, newStore.objects) || !reflect.DeepEqual(oldCalls, newCalls) || len(newTrace) >= len(oldTrace) {
		test.Fatal("nested, hidden or large block capture changed its original wire or failed to reduce steps")
	}
	legacyNormalizationCaptureRQ87Idle(test, decoder.mutation)
}

func TestLegacyNativeEventValuesAdministrativeLayoutBoundary(test *testing.T) {
	record := legacyNativeEventValue{event: CIREvent{Output: make([]any, 32_769)}, heap: docRangeHeaderHeapEstimate{bytes: 1 << 20}}
	list := legacyNativeEventRecordFieldsTestList(test, record)
	var states [2]string
	var progressed [2]int
	for index := range states {
		storage := legacyNativeGoGraphSetW85Storage(test)
		work, err := newLegacyNativeEventValuesCheckpoint(context.Background(), list, HashContent([]byte("layout-boundary")), 4096, storage.io, legacyDecoderCheckpointBudget)
		if err != nil {
			test.Fatal(err)
		}
		defer work.close()
		for attempt := 0; attempt < 8 && (work.fields == nil || work.fields.graph.layout == nil); attempt++ {
			if ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, legacyEventValuesAdministrativeOriginal{work}, storage); err != nil || ready {
				test.Fatal("fixture missed the original layout boundary", ready, err)
			}
		}
		if work.fields == nil || work.fields.graph.layout == nil || work.fields.graph.layout.ready {
			test.Fatal("fixture did not retain unfinished GoLayout traversal")
		}
		before := 0
		if frame := work.fields.graph.layout.frame; frame != nil {
			before = frame.position
		}
		if index == 0 {
			for range 8 {
				if _, err = legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, legacyEventValuesAdministrativeOriginal{work}, storage); err != nil {
					break
				}
			}
		} else {
			_, err = legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, work, storage)
		}
		if err != nil || storage.reads != 0 || storage.writes != 0 || work.fields.graph.layout == nil || work.fields.graph.layout.ready {
			test.Fatal("pure layout quantum crossed into subsequent work", err)
		}
		states[index] = legacyEventValuesAdministrativeState(test, work)
		frame := work.fields.graph.layout.frame
		if frame == nil {
			test.Fatal("fixture did not advance into an active layout cursor")
		}
		progressed[index] = frame.position - before
	}
	if states[0] != states[1] || progressed[0] <= 0 || progressed[0] != progressed[1] {
		test.Fatal("zero-IO layout work differs from eight original quanta")
	}
}

func TestLegacyNativeEventValuesAdministrativePerTransitionFences(test *testing.T) {
	for _, phase := range []uint8{1, 2} {
		for _, scenario := range []string{"cancel", "budget", "source", "ledger"} {
			test.Run(fmt.Sprintf("phase%d/%s", phase, scenario), func(test *testing.T) {
				list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{})
				storage := legacyNativeGoGraphSetW85Storage(test)
				work, err := newLegacyNativeEventValuesCheckpoint(context.Background(), list, HashContent([]byte("administrative-fences")), 1024, storage.io, legacyDecoderCheckpointBudget)
				if err != nil {
					test.Fatal(err)
				}
				defer work.close()
				injected := false
				want := ErrConflict
				ctx := legacyNativeGoGraphSetTrivialBatchContext{Context: context.Background(), check: func() error {
					if injected || work.current == nil || work.current.phase != phase {
						return nil
					}
					injected = true
					switch scenario {
					case "cancel":
						want = context.Canceled
						return want
					case "budget":
						work.budget, want = 0, ErrDocumentResourceLimit
					case "source":
						list.mutation.stop()
					case "ledger":
						list.values.payload.bytes++
					}
					return nil
				}}
				storage.current = ctx
				if ready, err := work.step(ctx); !injected || ready || !errors.Is(err, want) || storage.reads != 0 || storage.writes != 0 {
					test.Fatal("coalesced administrative transition bypassed a fresh fence", injected, ready, err, want)
				}
				legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, work, storage, want)
				if scenario == "source" {
					if !list.mutation.closed.Load() || list.mutation.activity.Load() != 0 || list.mutation.capture.Load() != nil || !errors.Is(list.mutation.enter(), ErrConflict) {
						test.Fatal("stopped source retained a capture or resumed mutation")
					}
				} else {
					legacyNormalizationCaptureRQ87Idle(test, list.mutation)
				}
			})
		}
	}
}

func TestLegacyNativeEventValuesBufferedIOBoundary(test *testing.T) {
	for _, scenario := range []string{"buffered", "failure", "cancel"} {
		test.Run(scenario, func(test *testing.T) {
			list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			attempts := 0
			var wanted error
			storage := legacyNativeCheckpointIO{Put: func(current context.Context, hash ContentHash, body []byte) error {
				attempts++
				if current != ctx || len(body) > LegacyCanonicalEncodingBytes || HashContent(body) != hash {
					test.Fatal("buffered IO lost current context or bounded content")
				}
				if scenario == "failure" {
					wanted = ErrPreparedDocRangeObject
					return wanted
				}
				if scenario == "cancel" {
					wanted = context.Canceled
					cancel()
				}
				return nil
			}}
			work, err := newLegacyNativeEventValuesCheckpoint(ctx, list, HashContent([]byte("buffered-io")), 1024, storage, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			if work.store.Read != nil {
				test.Fatal("nil upstream reader became available")
			}
			ready, err := work.step(ctx)
			if ready || attempts != 1 || !errors.Is(err, wanted) {
				test.Fatal("capture crossed its first buffered or failed IO attempt", ready, attempts, err, wanted)
			}
			if wanted == nil {
				if work.fields == nil || work.fields.graph == nil || !work.fields.graph.ready || !work.attemptedIO {
					test.Fatal("buffered put consumed the graph-ready handoff")
				}
			} else if retained, failure := work.retainedBytes(); failure != nil || retained != 0 {
				test.Fatal("failed IO retained capture ownership", retained, failure)
			}
		})
	}
}
