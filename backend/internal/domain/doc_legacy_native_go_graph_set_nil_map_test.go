package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestLegacyNativeGoGraphSetNilMapWireAndOwners(test *testing.T) {
	var absent map[string]any
	empty := map[string]any{}
	shared := map[string]any{"value": "child"}
	maximum := make([]any, 32)
	for index := range maximum {
		maximum[index] = absent
	}
	if !legacyNativeCheckpointGoGraphSetScalar(absent) || legacyNativeCheckpointGoGraphSetScalar(empty) ||
		legacyNativeCheckpointGoGraphSetScalar(shared) || legacyNativeCheckpointGoGraphSetScalar([]any(nil)) {
		test.Fatal("nil-map classification changed non-nil maps or slice handling")
	}
	for _, fixture := range []struct {
		name  string
		roots []any
	}{
		{name: "nil", roots: []any{absent}},
		{name: "maximum", roots: maximum},
		{name: "empty-alias", roots: []any{absent, empty, empty, absent}},
		{name: "nonempty-alias", roots: []any{absent, shared, shared, absent}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			binding := HashContent([]byte("nil-map-wire-oracle"))
			original, optimized := legacyNativeGoGraphSetW85Storage(test), legacyNativeGoGraphSetW85Storage(test)
			before, oldIdentity, oldSteps := legacyNativeGoGraphSetTrivialCapture(test, fixture.roots, binding, original, true)
			after, newIdentity, newSteps := legacyNativeGoGraphSetTrivialCapture(test, fixture.roots, binding, optimized, false)
			if before != after || oldIdentity != newIdentity || original.writes != optimized.writes || original.transferred != optimized.transferred || !reflect.DeepEqual(original.objects, optimized.objects) {
				test.Fatal("nil-map fast path changed wire, hashes, CAS bytes or identity increments")
			}
			if newSteps >= oldSteps || fixture.name == "maximum" && (newSteps != 4 || newIdentity != 32 || optimized.writes != 1) {
				test.Fatal("nil maps escaped the fixed batch or retained generic microsteps", oldSteps, newSteps, newIdentity, optimized.writes)
			}
			var state legacyNativeCheckpointGoGraphSetState
			if err := json.Unmarshal(optimized.objects[after.Hash], &state); err != nil {
				test.Fatal(err)
			}
			bounds := legacyNativeValueCheckpointBounds{PayloadBytes: legacyNativeValueCheckpointPayloadLimit(), Nodes: legacyNativeCheckpointNodes}
			for index, root := range fixture.roots {
				if value, ok := root.(map[string]any); !ok || value != nil {
					continue
				}
				saved := state.Roots[index]
				wire, err := decodeLegacyNativeValueCheckpoint(context.Background(), saved.Wire, bounds)
				if err != nil || wire != (legacyNativeValueCheckpoint{Version: 1, Kind: legacyNativeCheckpointObject, Nil: true}) ||
					saved.Inline != nil || saved.Text != (legacyNativeCheckpointText{}) || saved.Body != (legacyNativeCheckpointRef{}) {
					test.Fatal("nil map lost its original kind, nil bit, zero reference/capacity or exclusive wire", wire, err)
				}
			}
			values := legacyNativeGoGraphSetW85Restore(test, after, binding, optimized, 0)
			oldValues := legacyNativeGoGraphSetW85Restore(test, before, binding, original, 0)
			legacyNativeGoGraphSetW85Values(test, fixture.roots, values)
			for index, root := range fixture.roots {
				if value, ok := root.(map[string]any); ok && value == nil &&
					(values[index].heap != oldValues[index].heap || values[index].excess != oldValues[index].excess || values[index].capacity != oldValues[index].capacity || values[index].owner != (legacyNativeAnyValueOwner{})) {
					test.Fatal("nil map changed its original heap, capacity or ownership")
				}
			}
		})
	}
}

func TestLegacyNativeGoGraphSetNilMapEventSteps(test *testing.T) {
	list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: CIREvent{Kind: EventMessage, Role: RoleAssistant}})
	storage := legacyNativeGoGraphSetW85Storage(test)
	fields, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, HashContent([]byte("nil-map-event-steps")), storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer fields.close()
	if err := list.mutation.enter(); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("event fields lost the real source mutation barrier", err)
	}
	graph := fields.graph
	steps, inputSteps := 0, 0
	for steps < legacyNativeGoGraphSetW85Steps && !graph.ready {
		if graph.position == 16 {
			inputSteps++
		}
		if _, err := legacyNativeGoGraphSetW85Step(test, graph, storage); err != nil {
			test.Fatal(err)
		}
		steps++
	}
	if !graph.ready || steps != 4 || inputSteps != 0 || graph.identity.nextID != 18 || graph.registry.first != nil || graph.identity.first != nil || storage.writes != 1 {
		test.Fatal("raw-message root schedule, identity or owner count changed", steps, inputSteps, graph.identity.nextID, storage.writes)
	}
	test.Logf("raw-message graph steps=%d root16 steps=%d CAS=%d identities=%d", steps, inputSteps, storage.writes, graph.identity.nextID)
	fields.close()
	if err := list.mutation.enter(); err != nil {
		test.Fatal("completed graph retained the source mutation barrier", err)
	}
	list.mutation.leave()
}

func TestLegacyNativeGoGraphSetNilMapPerRootFences(test *testing.T) {
	for _, phase := range []string{"layout", "wire"} {
		for _, scenario := range []string{"source", "budget", "cancel", "identity-limit", "identity-closed", "identity-failure", "registry-closed", "registry-failure", "registry-active"} {
			test.Run(phase+"/"+scenario, func(test *testing.T) {
				roots := []any{map[string]any(nil), map[string]any(nil), map[string]any(nil)}
				storage := legacyNativeGoGraphSetW85Storage(test)
				work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, HashContent([]byte("nil-map-per-root-fence")), storage.io, legacyNativeGoGraphSetW85Budget)
				if err != nil {
					test.Fatal(err)
				}
				defer work.close()
				if phase == "wire" {
					legacyNativeGoGraphSetTrivialLayout(test, work, storage)
				} else if ready, err := work.step(context.Background()); ready || err != nil || work.layout == nil {
					test.Fatal("layout setup", ready, err)
				}
				injected, expected := false, ErrIntegrity
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
						roots[1], expected = map[string]any{}, ErrConflict
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
					test.Fatal("nil-map batch bypassed a current per-root fence", injected, ready, err, expected)
				}
				legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, work, storage, expected)
			})
		}
	}
}

func TestLegacyNativeGoGraphSetNilMapCapturePerRoot(test *testing.T) {
	for _, phase := range []string{"layout", "wire"} {
		test.Run(phase, func(test *testing.T) {
			list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: CIREvent{Kind: EventMessage, Role: RoleAssistant}})
			storage := legacyNativeGoGraphSetW85Storage(test)
			fields, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, HashContent([]byte("nil-map-capture")), storage.io, legacyNativeGoGraphSetW85Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer fields.close()
			graph := fields.graph
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
				if !injected && position == 16 {
					injected = true
					list.mutation.stop()
				}
				return nil
			}}
			if ready, err := graph.step(ctx); !injected || ready || !errors.Is(err, ErrConflict) || storage.reads != 0 || storage.writes != 0 {
				test.Fatal("nil Input bypassed capture revocation at its own root", injected, ready, err)
			}
			legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, graph, storage, ErrConflict)
		})
	}
}

func TestLegacyNativeGoGraphSetNilMapAfterOwner(test *testing.T) {
	shared := map[string]any{"child": true}
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), []any{shared, map[string]any(nil)}, HashContent([]byte("nil-map-after-owner")), storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	for steps := 0; steps < legacyNativeGoGraphSetW85Steps && work.position != 1; steps++ {
		if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); ready || err != nil {
			test.Fatal("owner capture failed", ready, err)
		}
	}
	if work.position != 1 || work.registry.first == nil || work.identity.first == nil {
		test.Fatal("missing registered owner before nil map")
	}
	identity := work.identity.nextID
	if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); ready || err != nil || work.position != 1 || work.prepare == nil || work.identity.nextID != identity {
		test.Fatal("nil map bypassed the existing owner's generic verification", ready, err)
	}
	shared["child"] = false
	legacyNativeGoGraphSetW85Reject[legacyNativeCheckpointRef](test, work, nil, storage, ErrConflict, legacyNativeGoGraphSetW85Budget)
}
