package domain

import (
	"context"
	"errors"
	"math"
	"reflect"
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

func TestLegacyNativeGoGraphSetTrivialOneRootPerStep(test *testing.T) {
	roots := []any{nil, false, true, "", math.Copysign(0, -1), math.MaxFloat64}
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, HashContent([]byte("trivial-root-steps")), storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	legacyNativeGoGraphSetTrivialLayout(test, work, storage)
	for position := range roots {
		before := work.state.retained()
		if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || ready || work.position != position+1 || work.identity.nextID != uint64(position+1) {
			test.Fatal("trivial root did not take exactly one bounded step", position, ready, err)
		}
		if work.prepare != nil || work.write != nil || work.phase != 0 || storage.reads != 0 || storage.writes != 0 || len(work.state.Roots[position].Wire) != legacyNativeValueCheckpointBytes {
			test.Fatal("trivial root created child work or payload IO")
		}
		if after := work.state.retained(); before.err != nil || after.err != nil || after.bytes <= before.bytes {
			test.Fatal("wire ownership was not charged", before, after)
		}
	}
	if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); err != nil || !ready || storage.writes != 1 {
		test.Fatal("set root did not retain its single bounded CAS write", ready, err, storage.writes)
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
