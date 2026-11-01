package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

func legacyNativeCheckpointGraphShortValue(text string) legacyNativeAnyValue {
	capacity := len(text) + 8
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(text)))
	heap.allocation(int64(capacity), 1)
	return legacyNativeAnyValue{value: text, capacity: capacity, heap: heap, excess: 16}
}

func legacyNativeCheckpointGraphShortCapture(test *testing.T, value legacyNativeAnyValue, original bool) (legacyNativeCheckpointValue, *legacyNativeGoGraphSetW85Store, int, uint64) {
	test.Helper()
	ctx := context.Background()
	storage := legacyNativeGoGraphSetW85Storage(test)
	storage.current = ctx
	identity := newLegacyNativeCheckpointGraphIdentity()
	defer identity.close()
	work, err := newLegacyNativeCheckpointGraphWriter(ctx, value, storage.io, identity, legacyNativeGoGraphW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	for steps := 1; steps <= legacyNativeGoGraphW85StepLimit; steps++ {
		frame := work.frame
		if original && frame != nil && frame.phase == legacyNativeCheckpointGraphCaptureValue && frame.state.Kind == legacyNativeCheckpointString {
			if err := work.admit(legacyNativeCheckpointGraphCaptureWorkspace); err != nil {
				test.Fatal(err)
			}
			frame.text, err = newLegacyNativeCheckpointTextWriter(ctx, int(frame.state.Length), frame.snapshot.read, storage.io)
			if err != nil {
				test.Fatal(err)
			}
			frame.phase = legacyNativeCheckpointGraphCaptureText
			if err := work.measure(frame); err != nil {
				test.Fatal(err)
			}
			continue
		}
		short := frame != nil && frame.phase == legacyNativeCheckpointGraphCaptureValue && frame.state.Kind == legacyNativeCheckpointString && frame.state.Length <= 64
		writes, transferred := storage.writes, storage.transferred
		ready, err := work.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if storage.writes-writes > 1 || storage.transferred-transferred > LegacyCanonicalEncodingBytes {
			test.Fatal("small graph step changed its single-object IO bound")
		}
		if retained, err := work.retainedBytes(); err != nil || retained <= 0 || retained > legacyNativeGoGraphW85Budget {
			test.Fatal("graph capture lost retained admission", retained, err)
		}
		if !original && short && frame == work.frame {
			test.Fatal("short snapshot retained a text continuation")
		}
		if ready {
			result, err := work.take(ctx)
			if err != nil {
				test.Fatal(err)
			}
			legacyNativeGoGraphSetW85Released[legacyNativeCheckpointValue](test, work)
			return result, storage, steps, identity.nextID
		}
	}
	test.Fatal("graph capture exhausted the existing finite schedule")
	return legacyNativeCheckpointValue{}, storage, 0, 0
}

func TestLegacyNativeCheckpointGraphShortSnapshotOracle(test *testing.T) {
	for _, text := range []string{"", "x", strings.Repeat("x", 63), strings.Repeat("x", 64), strings.Repeat("x", 65), "\x00\ud55c\U0001f642\xff\"\\\n<>&"} {
		test.Run(strconv.Itoa(len(text)), func(test *testing.T) {
			value := legacyNativeCheckpointGraphShortValue(text)
			expected, before, oldSteps, oldIdentity := legacyNativeCheckpointGraphShortCapture(test, value, true)
			actual, after, newSteps, newIdentity := legacyNativeCheckpointGraphShortCapture(test, value, false)
			inline := len(text) > 0 && len(text) <= 64 && value.capacity <= 64
			if inline {
				if !bytes.Equal(actual.Wire, expected.Wire) || string(actual.Inline) != text || actual.Text != (legacyNativeCheckpointText{}) || actual.Body != expected.Body || before.writes != 1 || after.writes != 0 || after.transferred != 0 || len(after.objects) != 0 || string(before.objects[expected.Text.Tree.Ref.Hash]) != text {
					test.Fatal("inline snapshot did not replace exactly the raw leaf CAS")
				}
			} else if !reflect.DeepEqual(actual, expected) || !reflect.DeepEqual(after.objects, before.objects) || after.writes != before.writes || after.transferred != before.transferred {
				test.Fatal("short snapshot changed wire, text tree, objects, capacity or identity")
			}
			if newIdentity != oldIdentity {
				test.Fatal("inline snapshot changed identity")
			}
			if len(text) <= 64 && newSteps >= oldSteps || len(text) > 64 && newSteps != oldSteps {
				test.Fatal("short snapshot did not remove only its empty transitions", oldSteps, newSteps)
			}
			bounds := legacyNativeValueCheckpointBounds{PayloadBytes: legacyNativeValueCheckpointPayloadLimit(), Nodes: legacyNativeCheckpointNodes}
			state, err := decodeLegacyNativeValueCheckpoint(context.Background(), actual.Wire, bounds)
			writes := min(len(text), 1)
			if inline {
				writes = 0
			}
			if err != nil || state.Length != uint64(len(text)) || state.Capacity != uint64(value.capacity) || after.writes != writes {
				test.Fatal("short snapshot changed logical/physical capacity or leaf writes", state, err, after.writes)
			}
			test.Logf("bytes=%d original_steps=%d candidate_steps=%d writes=%d", len(text), oldSteps, newSteps, after.writes)
		})
	}
}

func TestLegacyNativeCheckpointGraphShortNestedOracle(test *testing.T) {
	shared := map[string]any{"z": "last", "a": "first"}
	source := map[string]any{"z": shared, "a": "ordinal-0001", "alias": shared}
	registry := newLegacyNativeCheckpointGoGraphRegistry()
	defer registry.close()
	value, _ := legacyNativeGoGraphW85Import(test, source, registry)
	expected, before, oldSteps, oldIdentity := legacyNativeCheckpointGraphShortCapture(test, value, true)
	actual, after, newSteps, newIdentity := legacyNativeCheckpointGraphShortCapture(test, value, false)
	if !bytes.Equal(actual.Wire, expected.Wire) || before.writes != 14 || after.writes != before.writes-8 || newIdentity != oldIdentity || newSteps >= oldSteps {
		test.Fatal("nested short snapshot changed owner/alias representation or failed to remove transitions", oldSteps, newSteps)
	}
	for _, fixture := range []struct {
		saved legacyNativeCheckpointValue
		store *legacyNativeGoGraphSetW85Store
	}{{expected, before}, {actual, after}} {
		restored := newLegacyNativeCheckpointGraphRestored()
		defer restored.close()
		reader, err := newLegacyNativeCheckpointGraphRestore(context.Background(), fixture.saved, fixture.store.io, restored, legacyNativeGoGraphW85Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer reader.close()
		legacyNativeGoGraphSetW85Ready[legacyNativeAnyValue](test, reader, fixture.store, legacyNativeGoGraphW85Budget)
		result, err := reader.take(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		inspector := &legacyNativeGoGraphW85Inspector{}
		inspector.check(test, source, result)
	}
	test.Logf("original_steps=%d candidate_steps=%d writes=%d identities=%d", oldSteps, newSteps, before.writes, newIdentity)
}

func TestLegacyNativeCheckpointGraphShortOwnerMutation(test *testing.T) {
	registry := newLegacyNativeCheckpointGoGraphRegistry()
	defer registry.close()
	value, _ := legacyNativeGoGraphW85Import(test, map[string]any{"key": "text"}, registry)
	store, _ := legacyNativeCheckpointStoreFixture(test)
	put := store.Put
	mutated := false
	store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		if !mutated {
			mutated = true
			if _, err := value.owner.object.putValueGuarded("key", 3, legacyNativeCheckpointGraphShortValue("changed"), nil); err != nil {
				return err
			}
		}
		return put(ctx, hash, body)
	}
	identity := newLegacyNativeCheckpointGraphIdentity()
	defer identity.close()
	work, err := newLegacyNativeCheckpointGraphWriter(context.Background(), value, store, identity, legacyNativeGoGraphW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	for steps := 0; steps < legacyNativeGoGraphW85StepLimit; steps++ {
		ready, err := work.step(context.Background())
		if err != nil {
			if !mutated || !errors.Is(err, ErrConflict) || ready {
				test.Fatal("short child lost parent owner mutation detection", mutated, ready, err)
			}
			legacyNativeCheckpointStringsOracleFailed(test, work, ErrConflict)
			return
		}
		if ready {
			test.Fatal("changed owner published a checkpoint")
		}
	}
	test.Fatal("owner mutation exceeded the existing finite schedule")
}

func TestLegacyNativeCheckpointGraphShortSnapshotFences(test *testing.T) {
	for _, scenario := range []string{"write", "cancel-before", "cancel-write", "snapshot-closed", "snapshot-length", "capacity", "budget", "identity-closed", "identity-failure"} {
		test.Run(scenario, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			storage := legacyNativeCheckpointStringsOracleStorage(test)
			identity := newLegacyNativeCheckpointGraphIdentity()
			defer identity.close()
			calls, wantCalls := 0, 0
			want := ErrIntegrity
			store := storage.io
			store.Put = func(current context.Context, hash ContentHash, body []byte) error {
				calls++
				if current != ctx || len(body) != 4 || hash != HashContent([]byte("text")) {
					test.Fatal("short snapshot changed invocation context or leaf bytes")
				}
				if scenario == "write" {
					return ErrConflict
				}
				if scenario == "cancel-write" {
					cancel()
				}
				return storage.io.Put(current, hash, body)
			}
			value := legacyNativeCheckpointGraphShortValue("text")
			if scenario == "write" || scenario == "cancel-write" {
				value.capacity = 65
				value.heap.allocation(65-12, 1)
				value.excess = 2 * (65 - 4)
			}
			work, err := newLegacyNativeCheckpointGraphWriter(ctx, value, store, identity, legacyNativeGoGraphW85Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			switch scenario {
			case "write":
				want, wantCalls = ErrConflict, 1
			case "cancel-before":
				cancel()
				want = context.Canceled
			case "cancel-write":
				want, wantCalls = context.Canceled, 1
			case "snapshot-closed":
				work.frame.snapshot.close()
			case "snapshot-length":
				work.frame.snapshot.text = "changed"
			case "capacity":
				work.frame.state.Capacity = 3
			case "budget":
				retained, err := work.retainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				shared, err := identity.retainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				work.budget, want = retained+shared, ErrDocumentResourceLimit
			case "identity-closed":
				identity.close()
			case "identity-failure":
				identity.failure, want = ErrConflict, ErrConflict
			}
			if ready, err := work.step(ctx); ready || !errors.Is(err, want) || calls != wantCalls {
				test.Fatal("short snapshot bypassed failure fence", ready, err, want, calls, wantCalls)
			}
			legacyNativeCheckpointStringsOracleFailed(test, work, want)
			if calls != wantCalls {
				test.Fatal("failed capture retried CAS")
			}
		})
	}
}
