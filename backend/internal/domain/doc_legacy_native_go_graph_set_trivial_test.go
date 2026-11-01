package domain

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func legacyNativeGoGraphSetTrivialLayout(test *testing.T, work *legacyNativeCheckpointGoGraphSet, storage *legacyNativeGoGraphSetW85Store) {
	test.Helper()
	for range legacyNativeGoGraphSetW85Steps {
		if work.layout != nil && work.layout.ready {
			return
		}
		if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || ready || work.position != 0 || work.identity.nextID != 0 || work.prepare != nil {
			test.Fatal("root capture ran before layout completion", ready, err)
		}
	}
	test.Fatal("layout exceeded its existing step limit")
}

func legacyNativeGoGraphSetTrivialCapture(test *testing.T, roots []any, binding ContentHash, storage *legacyNativeGoGraphSetW85Store, original bool) (legacyNativeCheckpointRef, uint64, int) {
	test.Helper()
	work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, binding, storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	for steps := 1; steps <= legacyNativeGoGraphSetW85Steps; steps++ {
		if original && work.phase == 0 && work.position < len(work.values) && work.prepare == nil {
			remaining, err := work.remaining()
			if err != nil {
				test.Fatal(err)
			}
			shared, err := work.registry.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			work.prepare, err = newLegacyNativeCheckpointGoGraph(context.Background(), work.values[work.position], work.registry, remaining+shared)
			if err != nil {
				test.Fatal(err)
			}
			continue
		}
		ready, err := legacyNativeGoGraphSetW85Step(test, work, storage)
		if err != nil {
			test.Fatal(err)
		}
		if retained, err := work.retainedBytes(); err != nil || retained > work.budget {
			test.Fatal("capture escaped its heap budget", retained, err)
		}
		if ready {
			identity := work.identity.nextID
			ref, err := work.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			legacyNativeGoGraphSetW85Released[legacyNativeCheckpointRef](test, work)
			return ref, identity, steps
		}
	}
	test.Fatal("capture exceeded its existing step limit")
	return legacyNativeCheckpointRef{}, 0, 0
}

func TestLegacyNativeGoGraphSetTrivialWireAndIdentity(test *testing.T) {
	shared := map[string]any{"value": true}
	array := []any{shared, ""}
	_, fields, err := legacyNativeEventRecordFieldsFor(CIREvent{Kind: EventToolCall, CallID: "replacement-00001", ToolName: "replacement-pages", Input: map[string]any{"value": "child"}}, HashContent([]byte("trivial-event-fields")))
	if err != nil {
		test.Fatal(err)
	}
	for _, fixture := range []struct {
		name  string
		roots []any
	}{
		{name: "scalars", roots: []any{nil, false, true, "", float64(0), math.Copysign(0, -1), math.SmallestNonzeroFloat64, math.MaxFloat64, -1.25}},
		{name: "fallback-and-aliases", roots: []any{nil, "", "\u0000\ud55c\U0001f642\xff", true, shared, shared, false, array, array, "", ([]any)(nil), (map[string]any)(nil)}},
		{name: "event-fields", roots: fields[:]},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			binding := HashContent([]byte("trivial-root-wire"))
			original, optimized := legacyNativeGoGraphSetW85Storage(test), legacyNativeGoGraphSetW85Storage(test)
			before, oldIdentity, oldSteps := legacyNativeGoGraphSetTrivialCapture(test, fixture.roots, binding, original, true)
			after, newIdentity, newSteps := legacyNativeGoGraphSetTrivialCapture(test, fixture.roots, binding, optimized, false)
			if before != after || oldIdentity != newIdentity || !reflect.DeepEqual(original.objects, optimized.objects) || original.writes != optimized.writes {
				test.Fatal("fast path changed wire, CAS hashes, leaf identity increments, or object writes", before, after, oldIdentity, newIdentity)
			}
			if newSteps >= oldSteps {
				test.Fatal("trivial roots did not eliminate state-machine steps", oldSteps, newSteps)
			}
			values := legacyNativeGoGraphSetW85Restore(test, after, binding, optimized, 0)
			legacyNativeGoGraphSetW85Values(test, fixture.roots, values)
			test.Log("original/fast steps", oldSteps, newSteps, "identities", newIdentity, "CAS writes", optimized.writes)
		})
	}
}

func TestLegacyNativeGoGraphSetTrivialBoundedRootBatch(test *testing.T) {
	values := []any{nil, false, true, "", math.Copysign(0, -1), math.MaxFloat64}
	roots := make([]any, 32)
	for position := range roots {
		roots[position] = values[position%len(values)]
	}
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, HashContent([]byte("trivial-root-steps")), storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	legacyNativeGoGraphSetTrivialLayout(test, work, storage)
	before := work.state.retained()
	if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || ready || work.position != len(roots) || work.identity.nextID != uint64(len(roots)) {
		test.Fatal("trivial roots did not retain the fixed 32-root step bound", ready, err)
	}
	var captured docRangeHeaderHeapEstimate
	for position := range roots {
		if work.prepare != nil || work.write != nil || work.phase != 0 || storage.reads != 0 || storage.writes != 0 || len(work.state.Roots[position].Wire) != legacyNativeValueCheckpointBytes {
			test.Fatal("trivial root created child work or payload IO")
		}
		legacyNativeCheckpointGraphCapturedValueHeap(&captured, work.state.Roots[position])
	}
	if after := work.state.retained(); before.err != nil || after.err != nil || captured.err != nil || after.bytes-before.bytes != captured.bytes || captured.bytes <= 0 {
		test.Fatal("all batched wire ownership was not charged exactly", before, after, captured)
	}
	if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || !ready || storage.writes != 1 {
		test.Fatal("set root did not retain its single bounded CAS write", ready, err, storage.writes)
	}
}

type legacyNativeGoGraphSetTrivialBatchContext struct {
	context.Context
	check func() error
}

func (ctx legacyNativeGoGraphSetTrivialBatchContext) Err() error {
	if err := ctx.Context.Err(); err != nil {
		return err
	}
	return ctx.check()
}

func TestLegacyNativeGoGraphSetTrivialBatchStopsAtIOAndOwners(test *testing.T) {
	shared := map[string]any{"child": true}
	roots := []any{nil, true, "text", "", false, shared, false, ""}
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, HashContent([]byte("trivial-batch-boundaries")), storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	legacyNativeGoGraphSetTrivialLayout(test, work, storage)
	for _, expected := range []struct{ position, writes, transferred int }{{2, 0, 0}, {3, 1, 4}, {5, 1, 4}} {
		if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || ready || work.position != expected.position ||
			storage.writes != expected.writes || storage.transferred != expected.transferred || work.prepare != nil || work.write != nil {
			test.Fatal("batch crossed a text CAS or owner boundary", expected, work.position, storage.writes, storage.transferred, ready, err)
		}
	}
	if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || ready || work.position != 5 || work.prepare == nil {
		test.Fatal("map root lost its original preparation operation", ready, err)
	}
	for range legacyNativeGoGraphSetW85Steps {
		if work.position == 6 {
			break
		}
		if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || ready {
			test.Fatal("map root did not finish", ready, err)
		}
	}
	if work.position != 6 || work.registry.first == nil || work.identity.first == nil {
		test.Fatal("missing registered root owner")
	}
	identity := work.identity.nextID
	if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || ready || work.position != 6 || work.prepare == nil || work.identity.nextID != identity {
		test.Fatal("registered root map allowed scalar fast path", ready, err)
	}
	shared["child"] = false
	legacyNativeGoGraphSetW85Reject[legacyNativeCheckpointRef](test, work, nil, storage, ErrConflict, legacyNativeGoGraphSetW85Budget)
}

func TestLegacyNativeGoGraphSetTrivialLayoutRootBatch(test *testing.T) {
	for _, roots := range [][]any{make([]any, 32), {nil, false, "text", true, map[string]any{"child": nil}, nil, false}} {
		storage := legacyNativeGoGraphSetW85Storage(test)
		work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, HashContent([]byte("trivial-layout-batch")), storage.io, legacyNativeGoGraphSetW85Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer work.close()
		if ready, err := work.step(context.Background()); ready || err != nil || work.layout == nil {
			test.Fatal("layout was not allocated independently", ready, err)
		}
		layout := work.layout
		expected := []int{32}
		if len(roots) != 32 {
			expected = []int{2, 3, 4, 5}
		}
		for _, position := range expected {
			if err := layout.discover(context.Background()); err != nil || layout.rootIndex != position || work.position != 0 || work.identity.nextID != 0 || storage.reads != 0 || storage.writes != 0 {
				test.Fatal("layout batched a nontrivial root or encoded roots", position, layout.rootIndex, err)
			}
		}
		if len(roots) == 32 {
			if layout.frame != nil || layout.root != nil || layout.visits != 0 || layout.phase != 0 {
				test.Fatal("plain roots created traversal or crossed the phase boundary")
			}
		} else {
			if layout.frame == nil || layout.root == nil {
				test.Fatal("map lost its traversal frame")
			}
			for steps := 0; steps < 4 && layout.frame != nil; steps++ {
				if err := layout.discover(context.Background()); err != nil || layout.rootIndex != 5 {
					test.Fatal("map traversal crossed the next root", err)
				}
			}
			if layout.frame != nil {
				test.Fatal("bounded map traversal did not finish")
			}
			if err := layout.discover(context.Background()); err != nil || layout.rootIndex != 6 {
				test.Fatal("layout batched roots after registering a map", err)
			}
		}
	}
}

func TestLegacyNativeGoGraphSetTrivialBatchPerRootFences(test *testing.T) {
	for _, phase := range []string{"layout", "wire"} {
		for _, scenario := range []string{"source", "budget", "cancel", "identity-limit", "identity-closed", "identity-failure", "registry-closed", "registry-failure", "registry-active"} {
			test.Run(phase+"/"+scenario, func(test *testing.T) {
				roots := []any{nil, false, "", float64(3)}
				storage := legacyNativeGoGraphSetW85Storage(test)
				work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, HashContent([]byte("trivial-batch-fences")), storage.io, legacyNativeGoGraphSetW85Budget)
				if err != nil {
					test.Fatal(err)
				}
				defer work.close()
				if phase == "wire" {
					legacyNativeGoGraphSetTrivialLayout(test, work, storage)
				} else if ready, err := work.step(context.Background()); ready || err != nil || work.layout == nil {
					test.Fatal("layout setup", ready, err)
				}
				injected := false
				expected := ErrIntegrity
				ctx := legacyNativeGoGraphSetTrivialBatchContext{Context: context.Background(), check: func() error {
					position := work.position
					if phase == "layout" {
						position = work.layout.rootIndex
					}
					if injected || position != 1 {
						return nil
					}
					injected = true
					switch scenario {
					case "source":
						roots[1], expected = true, ErrConflict
					case "budget":
						work.budget, expected = 0, ErrDocumentResourceLimit
					case "cancel":
						expected = context.Canceled
						return expected
					case "identity-limit":
						work.identity.nextID = legacyNativeCheckpointNodes
					case "identity-closed":
						work.identity.close()
					case "identity-failure":
						work.identity.fail(ErrConflict)
						expected = ErrConflict
					case "registry-closed":
						work.registry.close()
					case "registry-failure":
						work.registry.fail(ErrConflict)
						expected = ErrConflict
					case "registry-active":
						work.registry.active, expected = &legacyNativeCheckpointGoGraph{}, ErrConflict
					}
					return nil
				}}
				if ready, err := work.step(ctx); !injected || ready || !errors.Is(err, expected) || storage.reads != 0 || storage.writes != 0 {
					test.Fatal("second root bypassed a current fence", injected, ready, err, expected)
				}
				legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, work, storage, expected)
			})
		}
	}
}

func TestLegacyNativeGoGraphSetTrivialBatchCurrentHeadroom(test *testing.T) {
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), []any{nil, false}, HashContent([]byte("trivial-current-headroom")), storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	legacyNativeGoGraphSetTrivialLayout(test, work, storage)
	retained, err := work.retainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	work.budget = retained + legacyNativeAnyCheckpointScratch
	if ready, err := work.step(context.Background()); ready || !errors.Is(err, ErrDocumentResourceLimit) || storage.writes != 0 {
		test.Fatal("second root borrowed the first root's stale headroom", ready, err)
	}
	legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, work, storage, ErrDocumentResourceLimit)
}

func TestLegacyNativeGoGraphSetTrivialBatchCapturePerRoot(test *testing.T) {
	for _, phase := range []string{"layout", "wire"} {
		test.Run(phase, func(test *testing.T) {
			list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: CIREvent{}})
			storage := legacyNativeGoGraphSetW85Storage(test)
			work, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, HashContent([]byte("trivial-batch-capture")), storage.io, legacyNativeGoGraphSetW85Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			graph := work.graph
			if phase == "wire" {
				legacyNativeGoGraphSetTrivialLayout(test, graph, storage)
			} else if ready, err := graph.step(context.Background()); ready || err != nil || graph.layout == nil {
				test.Fatal("layout setup", ready, err)
			}
			injected := false
			ctx := legacyNativeGoGraphSetTrivialBatchContext{Context: context.Background(), check: func() error {
				position := graph.position
				if phase == "layout" {
					position = graph.layout.rootIndex
				}
				if !injected && position == 1 {
					injected = true
					list.mutation.stop()
				}
				return nil
			}}
			if ready, err := graph.step(ctx); !injected || ready || !errors.Is(err, ErrConflict) || storage.writes != 0 {
				test.Fatal("batched field advanced after capture revocation", injected, ready, err)
			}
		})
	}
}

func TestLegacyNativeGoGraphSetTrivialFences(test *testing.T) {
	for _, scenario := range []string{"source", "budget", "cancel", "identity-limit", "identity-closed", "identity-failure", "registry-closed", "registry-failure", "registry-active"} {
		test.Run(scenario, func(test *testing.T) {
			roots := []any{""}
			storage := legacyNativeGoGraphSetW85Storage(test)
			work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, HashContent([]byte("trivial-root-fences")), storage.io, legacyNativeGoGraphSetW85Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			legacyNativeGoGraphSetTrivialLayout(test, work, storage)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expected := ErrIntegrity
			switch scenario {
			case "source":
				roots[0], expected = "changed", ErrConflict
			case "budget":
				retained, err := work.retainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				work.budget, expected = retained+legacyNativeAnyCheckpointScratch-1, ErrDocumentResourceLimit
			case "cancel":
				cancel()
				expected = context.Canceled
			case "identity-limit":
				work.identity.nextID = legacyNativeCheckpointNodes
			case "identity-closed":
				work.identity.close()
			case "identity-failure":
				work.identity.fail(ErrConflict)
				expected = ErrConflict
			case "registry-closed":
				work.registry.close()
			case "registry-failure":
				work.registry.fail(ErrConflict)
				expected = ErrConflict
			case "registry-active":
				work.registry.active, expected = &legacyNativeCheckpointGoGraph{}, ErrConflict
			}
			if ready, err := work.step(ctx); ready || !errors.Is(err, expected) || storage.writes != 0 {
				test.Fatal("fast path bypassed a fence", ready, err, expected)
			}
			legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, work, storage, expected)
		})
	}
}

func TestLegacyNativeGoGraphSetTrivialPreservesOwnerVerification(test *testing.T) {
	shared := map[string]any{"child": true}
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), []any{shared, ""}, HashContent([]byte("trivial-owner-fence")), storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	for range legacyNativeGoGraphSetW85Steps {
		if work.position == 1 {
			break
		}
		if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || ready {
			test.Fatal("owner was not captured before the trivial root", ready, err)
		}
	}
	if work.position != 1 || work.registry.first == nil || work.identity.first == nil {
		test.Fatal("fixture did not retain existing owner verification")
	}
	shared["child"] = false
	legacyNativeGoGraphSetW85Reject[legacyNativeCheckpointRef](test, work, nil, storage, ErrConflict, legacyNativeGoGraphSetW85Budget)
}

func TestLegacyNativeGoGraphSetTrivialRecordCaptureFences(test *testing.T) {
	for _, scenario := range []string{"source", "capture", "binding"} {
		test.Run(scenario, func(test *testing.T) {
			list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: CIREvent{}})
			storage := legacyNativeGoGraphSetW85Storage(test)
			work, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, HashContent([]byte("trivial-record-fences")), storage.io, legacyNativeGoGraphSetW85Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			legacyNativeGoGraphSetTrivialLayout(test, work.graph, storage)
			switch scenario {
			case "source":
				work.record.event.ID = "changed"
			case "capture":
				list.mutation.stop()
			case "binding":
				work.state.Binding = HashContent([]byte("changed"))
			}
			if ready, err := work.graph.step(context.Background()); ready || !errors.Is(err, ErrConflict) || storage.writes != 0 {
				test.Fatal("trivial field bypassed source capture", ready, err)
			}
		})
	}
}

func TestLegacyNativeGoGraphSetShortScalarWireOracle(test *testing.T) {
	for _, fixture := range []struct {
		name string
		text string
	}{
		{name: "empty"},
		{name: "one-byte", text: "x"},
		{name: "exact-bytes", text: "\x00\ud55c\U0001f642\xff\xfe\"\\\n<>&"},
		{name: "below-leaf", text: strings.Repeat("x", LegacyCanonicalEncodingBytes-1)},
		{name: "full-leaf", text: strings.Repeat("x", LegacyCanonicalEncodingBytes)},
		{name: "multi-leaf-fallback", text: strings.Repeat("x", LegacyCanonicalEncodingBytes+1)},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			binding := HashContent([]byte("short-scalar-wire-oracle"))
			original, optimized := legacyNativeGoGraphSetW85Storage(test), legacyNativeGoGraphSetW85Storage(test)
			roots := []any{fixture.text}
			before, oldIdentity, oldSteps := legacyNativeGoGraphSetTrivialCapture(test, roots, binding, original, true)
			after, newIdentity, newSteps := legacyNativeGoGraphSetTrivialCapture(test, roots, binding, optimized, false)
			if before != after || oldIdentity != 1 || newIdentity != oldIdentity || original.writes != optimized.writes || !reflect.DeepEqual(original.objects, optimized.objects) {
				test.Fatal("short scalar changed the generic writer wire, Text, CAS bytes, or identity")
			}
			var state legacyNativeCheckpointGoGraphSetState
			if err := json.Unmarshal(original.objects[before.Hash], &state); err != nil || len(state.Roots) != 1 {
				test.Fatal("generic writer root is invalid", err)
			}
			bounds := legacyNativeValueCheckpointBounds{PayloadBytes: legacyNativeValueCheckpointPayloadLimit(), Nodes: legacyNativeCheckpointNodes}
			wire, err := decodeLegacyNativeValueCheckpoint(context.Background(), state.Roots[0].Wire, bounds)
			length := len(fixture.text)
			if err != nil || wire != (legacyNativeValueCheckpoint{Version: 1, Kind: legacyNativeCheckpointString, Length: uint64(length), Capacity: uint64(length)}) {
				test.Fatal("generic root string capacity differs from source length", wire, err)
			}
			if length <= LegacyCanonicalEncodingBytes {
				expected := legacyNativeCheckpointText{}
				if length > 0 {
					expected = legacyNativeCheckpointText{Bytes: length, Tree: legacyNativeCheckpointTree{Ref: legacyNativeCheckpointRef{Hash: HashContent([]byte(fixture.text)), Bytes: length}, Count: uint64(length)}}
				}
				if state.Roots[0].Text != expected || state.Roots[0].Body != (legacyNativeCheckpointRef{}) {
					test.Fatal("generic short root does not use the existing single-leaf Text form")
				}
			}
			expectedValues := legacyNativeGoGraphSetW85Restore(test, before, binding, original, 0)
			values := legacyNativeGoGraphSetW85Restore(test, after, binding, optimized, 0)
			legacyNativeGoGraphSetW85Values(test, roots, values)
			if values[0].capacity != expectedValues[0].capacity || values[0].heap != expectedValues[0].heap || values[0].excess != expectedValues[0].excess {
				test.Fatal("restored short scalar changed generic reader capacity or heap", values[0], expectedValues[0])
			}
			test.Log("original/current steps", oldSteps, newSteps, "CAS writes", optimized.writes)
		})
	}
}

func TestLegacyNativeGoGraphSetShortScalarOneLeafPerStep(test *testing.T) {
	roots := []any{"kind", "\x00\ud55c\U0001f642\xff", strings.Repeat("x", LegacyCanonicalEncodingBytes)}
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, HashContent([]byte("short-scalar-steps")), storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	legacyNativeGoGraphSetTrivialLayout(test, work, storage)
	for position, root := range roots {
		before, err := work.retainedBytes()
		if err != nil {
			test.Fatal(err)
		}
		writes, transferred := storage.writes, storage.transferred
		if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || ready || work.position != position+1 || work.identity.nextID != uint64(position+1) {
			test.Fatal("short scalar did not complete in one step", position, ready, err)
		}
		if work.prepare != nil || work.write != nil || work.phase != 0 || storage.reads != 0 || storage.writes-writes != 1 || storage.transferred-transferred != len(root.(string)) {
			test.Fatal("short scalar exceeded its one-leaf byte/operation bound")
		}
		var expected docRangeHeaderHeapEstimate
		legacyNativeCheckpointGraphCapturedValueHeap(&expected, work.state.Roots[position])
		after, err := work.retainedBytes()
		if err != nil || expected.err != nil || after-before != expected.bytes {
			test.Fatal("short scalar wire/hash ownership changed retained heap", before, after, expected, err)
		}
	}
	if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || !ready || storage.writes != len(roots)+1 {
		test.Fatal("short scalar set changed final root publication", ready, err)
	}
}

func TestLegacyNativeGoGraphSetShortScalarIOFences(test *testing.T) {
	for _, scenario := range []string{"budget", "cancel", "put", "put-cancel", "put-source-change", "put-budget-change", "identity-limit"} {
		test.Run(scenario, func(test *testing.T) {
			roots := []any{"short"}
			storage := legacyNativeGoGraphSetW85Storage(test)
			work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, HashContent([]byte("short-scalar-io-fences")), storage.io, legacyNativeGoGraphSetW85Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			legacyNativeGoGraphSetTrivialLayout(test, work, storage)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expected, writes := ErrConflict, 1
			switch scenario {
			case "budget":
				retained, err := work.retainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				work.budget = retained + legacyNativeAnyCheckpointScratch - 1
				expected, writes = ErrDocumentResourceLimit, 0
			case "cancel":
				cancel()
				expected, writes = context.Canceled, 0
			case "put":
				storage.failure = ErrConflict
			case "put-cancel":
				storage.cancelIO, expected = cancel, context.Canceled
			case "put-source-change", "put-budget-change":
				put := work.store.Put
				work.store.Put = func(current context.Context, hash ContentHash, body []byte) error {
					err := put(current, hash, body)
					if scenario == "put-source-change" {
						roots[0] = "changed"
					} else {
						work.budget = 0
					}
					return err
				}
				if scenario == "put-budget-change" {
					expected = ErrDocumentResourceLimit
				}
			case "identity-limit":
				work.identity.nextID = legacyNativeCheckpointNodes
				expected, writes = ErrIntegrity, 0
			}
			storage.current = ctx
			if ready, err := work.step(ctx); ready || !errors.Is(err, expected) || storage.writes != writes || storage.reads != 0 {
				test.Fatal("short scalar bypassed IO/source/budget failure", ready, err, expected, storage.writes, writes)
			}
			storage.current = nil
			legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, work, storage, expected)
		})
	}
}

func TestLegacyNativeGoGraphSetShortScalarCaptureRevokedDuringPut(test *testing.T) {
	list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: CIREvent{Kind: EventToolCall}})
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, HashContent([]byte("short-scalar-capture")), storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	legacyNativeGoGraphSetTrivialLayout(test, work.graph, storage)
	put := work.graph.store.Put
	work.graph.store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		err := put(ctx, hash, body)
		list.mutation.stop()
		return err
	}
	if ready, err := legacyNativeGoGraphSetW85Step(test, work.graph, storage); ready || !errors.Is(err, ErrConflict) || storage.writes != 1 {
		test.Fatal("short scalar published after source capture revocation", ready, err, storage.writes)
	}
	legacyNativeGoGraphSetW85Released[legacyNativeCheckpointRef](test, work.graph)
}

func TestLegacyNativeGoGraphSetShortScalarOwnerFallback(test *testing.T) {
	shared := map[string]any{"child": true}
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), []any{shared, "short"}, HashContent([]byte("short-scalar-owner")), storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	for range legacyNativeGoGraphSetW85Steps {
		if work.position == 1 {
			break
		}
		if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || ready {
			test.Fatal("owner was not captured before short scalar", ready, err)
		}
	}
	if work.position != 1 || work.registry.first == nil || work.identity.first == nil {
		test.Fatal("fixture did not reach short scalar after owner capture")
	}
	shared["child"] = false
	legacyNativeGoGraphSetW85Reject[legacyNativeCheckpointRef](test, work, nil, storage, ErrConflict, legacyNativeGoGraphSetW85Budget)
}
