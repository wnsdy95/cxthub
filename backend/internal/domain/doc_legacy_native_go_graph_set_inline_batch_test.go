package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyNativeGoGraphSetInlineBatchEncodingAndAliases(test *testing.T) {
	maximum := make([]any, 32)
	for index := range maximum {
		maximum[index] = strings.Repeat("\xff", 64)
	}
	shared := map[string]any{"raw": "\x00\xff\ud55c\U0001f642"}
	for _, roots := range [][]any{maximum, {"first", "second", nil, "", shared, shared, "after-owner"}} {
		binding := HashContent([]byte("inline-batch-oracle"))
		original, batched := legacyNativeGoGraphSetW85Storage(test), legacyNativeGoGraphSetW85Storage(test)
		before, oldIdentity, _ := legacyNativeGoGraphSetTrivialCapture(test, roots, binding, original, true)
		after, newIdentity, _ := legacyNativeGoGraphSetTrivialCapture(test, roots, binding, batched, false)
		if before != after || newIdentity != oldIdentity || original.writes != batched.writes || original.transferred != batched.transferred || !reflect.DeepEqual(original.objects, batched.objects) {
			test.Fatal("inline batching changed wire, CAS objects, bytes or identity")
		}
		values := legacyNativeGoGraphSetW85Restore(test, after, binding, batched, 0)
		legacyNativeGoGraphSetW85Values(test, roots, values)
	}
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), maximum, HashContent([]byte("inline-batch-32")), storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	legacyNativeGoGraphSetTrivialLayout(test, work, storage)
	before := work.state.retained()
	if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); ready || err != nil || work.position != 32 || work.identity.nextID != 32 || storage.reads != 0 || storage.writes != 0 {
		test.Fatal("32 inline roots did not complete in one IO-free step", ready, err)
	}
	var captured docRangeHeaderHeapEstimate
	for _, root := range work.state.Roots {
		if len(root.Inline) != 64 || root.Text != (legacyNativeCheckpointText{}) || root.Body != (legacyNativeCheckpointRef{}) || len(root.Wire) != legacyNativeValueCheckpointBytes {
			test.Fatal("batched root lost its exclusive bounded inline encoding")
		}
		legacyNativeCheckpointGraphCapturedValueHeap(&captured, root)
	}
	if after := work.state.retained(); before.err != nil || after.err != nil || captured.err != nil || after.bytes-before.bytes != captured.bytes {
		test.Fatal("batched inline allocations were not charged exactly", before, after, captured)
	}
	if ready, err := legacyNativeGoGraphSetW85Step(test, work, storage); !ready || err != nil || storage.writes != 1 || storage.transferred > LegacyCanonicalEncodingBytes {
		test.Fatal("inline batch crossed the separate bounded root CAS", ready, err)
	}
	tooMany, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), append(maximum, "extra"), HashContent([]byte("inline-batch-33")), storage.io, legacyNativeGoGraphSetW85Budget)
	if tooMany != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("inline batching admitted a 33rd root", tooMany, err)
	}
}

func TestLegacyNativeGoGraphSetInlineBatchEventPrefix(test *testing.T) {
	number := json.Number("1.25e2")
	for _, fixture := range []struct {
		name  string
		event CIREvent
	}{
		{name: "replacement", event: CIREvent{Kind: EventToolCall, CallID: "child-0001", ToolName: "replacement", Input: map[string]any{"a": "first"}}},
		{name: "message", event: CIREvent{Kind: EventMessage, Role: RoleAssistant}},
		{name: "create-time", event: CIREvent{Kind: EventToolCall, CallID: "child-0001", ToolName: "replacement", ProviderMetadata: &ProviderMetadata{CreateTime: &number}}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: fixture.event})
			storage := legacyNativeGoGraphSetW85Storage(test)
			fields, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, HashContent([]byte("inline-event-prefix")), storage.io, legacyNativeGoGraphSetW85Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer fields.close()
			if err := list.mutation.enter(); !errors.Is(err, ErrLegacyNormalizationBusy) {
				test.Fatal("inline batching lost the actual capture barrier", err)
			}
			graph := fields.graph
			prefix := 16
			if fields.state.HasCreateTime {
				prefix = legacyCanonicalSourceNumberRoot
			}
			if ready, err := legacyNativeGoGraphSetW85Step(test, graph, storage); ready || err != nil || graph.layout == nil {
				test.Fatal("layout constructor crossed its original boundary", ready, err)
			}
			if err := graph.layout.discover(context.Background()); err != nil || graph.layout.rootIndex != prefix || graph.position != 0 || graph.identity.nextID != 0 || storage.writes != 0 {
				test.Fatal("layout did not batch exactly the inline prefix using rootIndex", prefix, err)
			}
			if fields.state.HasCreateTime {
				if err := graph.layout.discover(context.Background()); err != nil || graph.layout.rootIndex != 16 || storage.writes != 0 {
					test.Fatal("numeric layout root lost its separate discovery step", err)
				}
			}
			legacyNativeGoGraphSetTrivialLayout(test, graph, storage)
			if ready, err := legacyNativeGoGraphSetW85Step(test, graph, storage); ready || err != nil || graph.position != prefix || graph.identity.nextID != uint64(prefix) || graph.prepare != nil || storage.writes != 0 {
				test.Fatal("capture did not batch exactly the IO-free event prefix", prefix, ready, err)
			}
			if fields.state.HasCreateTime {
				if ready, err := legacyNativeGoGraphSetW85Step(test, graph, storage); ready || err != nil || graph.position != 16 || graph.identity.nextID != 16 || storage.writes != 1 {
					test.Fatal("CreateTime text CAS entered the inline batch", ready, err)
				}
				saved := graph.state.Roots[legacyCanonicalSourceNumberRoot]
				if saved.Inline != nil || saved.Text.Bytes != len(number) || string(storage.objects[saved.Text.Tree.Ref.Hash]) != string(number) {
					test.Fatal("numeric root lost its original Text fallback")
				}
			}
			fields.close()
			if err := list.mutation.enter(); err != nil {
				test.Fatal("inline prefix close retained its mutation barrier", err)
			}
			list.mutation.leave()
		})
	}
}

func TestLegacyNativeGoGraphSetInlineBatchPerRootFences(test *testing.T) {
	for _, phase := range []string{"layout", "wire"} {
		for _, scenario := range []string{"source", "budget", "cancel", "identity-limit", "identity-closed", "identity-failure", "registry-closed", "registry-failure", "registry-active"} {
			test.Run(phase+"/"+scenario, func(test *testing.T) {
				roots := []any{"first", strings.Repeat("\xff", 64), "last"}
				storage := legacyNativeGoGraphSetW85Storage(test)
				work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, HashContent([]byte("inline-batch-fence")), storage.io, legacyNativeGoGraphSetW85Budget)
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
						roots[1], expected = "changed", ErrConflict
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
					test.Fatal("second inline root bypassed a current fence", injected, ready, err, expected)
				}
				legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, work, storage, expected)
			})
		}
	}
}

func TestLegacyNativeGoGraphSetInlineBatchCurrentHeadroom(test *testing.T) {
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), []any{strings.Repeat("a", 64), strings.Repeat("b", 64)}, HashContent([]byte("inline-batch-headroom")), storage.io, legacyNativeGoGraphSetW85Budget)
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
		test.Fatal("second inline root borrowed stale headroom", ready, err)
	}
	legacyNativeGoGraphSetW85Failed[legacyNativeCheckpointRef](test, work, storage, ErrDocumentResourceLimit)
}
