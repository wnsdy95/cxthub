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

type legacyNativeGraphAdministrativeOriginal struct {
	*legacyNativeCheckpointGraphWriter
}

func (original legacyNativeGraphAdministrativeOriginal) step(ctx context.Context) (bool, error) {
	work := original.legacyNativeCheckpointGraphWriter
	if work == nil {
		return false, ErrIntegrity
	}
	if work.failure != nil {
		return false, work.failure
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if work.closed || work.identity == nil {
		return false, ErrIntegrity
	}
	if work.identity.failure != nil {
		return work.fail(work.identity.failure)
	}
	if work.identity.closed {
		return work.fail(ErrIntegrity)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	if work.ready {
		if work.root != nil {
			if err := work.root.validate(); err != nil {
				return work.fail(err)
			}
		}
		return true, nil
	}
	if work.verifying {
		if work.verify != nil {
			if !work.verify.finished {
				return work.fail(ErrConflict)
			}
			if err := work.verify.validate(); err != nil {
				return work.fail(err)
			}
			work.verify = work.verify.next
			return false, nil
		}
		work.ready = true
		return true, nil
	}
	frame := work.frame
	if frame == nil {
		return work.fail(ErrIntegrity)
	}
	if frame.captured != nil {
		if err := frame.captured.validate(); err != nil {
			return work.fail(err)
		}
	}
	if err := work.stepFrame(ctx, frame); err != nil {
		return work.fail(err)
	}
	if work.frame == frame {
		if err := work.measure(frame); err != nil {
			return work.fail(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return work.fail(err)
	}
	if err := work.admit(0); err != nil {
		return work.fail(err)
	}
	return false, nil
}

func legacyNativeGraphAdministrativeBoundary(work *legacyNativeCheckpointGraphWriter) string {
	if work.verifying {
		if work.verify != nil {
			return "verify"
		}
		return ""
	}
	frame := work.frame
	switch frame.phase {
	case legacyNativeCheckpointGraphCaptureValue:
		if frame.state.Kind == legacyNativeCheckpointString && frame.state.Length <= 64 {
			return "inline-or-leaf"
		}
	case legacyNativeCheckpointGraphCaptureText:
		return "text"
	case legacyNativeCheckpointGraphCaptureMembers:
		if frame.members != nil {
			if frame.captured.owner.array == nil {
				return "cursor"
			}
			if frame.position < frame.captured.record.logicalLength() {
				return "array"
			}
			if frame.position < frame.captured.record.Length {
				return "tail"
			}
		}
	case legacyNativeCheckpointGraphCaptureAppend:
		return "append"
	case legacyNativeCheckpointGraphCaptureMembersFinish:
		return "members-finish"
	case legacyNativeCheckpointGraphCaptureDirty:
		if frame.dirty != nil {
			return "dirty-cursor"
		}
	case legacyNativeCheckpointGraphCaptureDirtyFinish:
		return "dirty-finish"
	case legacyNativeCheckpointGraphCaptureOwner:
		return "owner"
	}
	return ""
}

func legacyNativeGraphAdministrativeState(test *testing.T, work *legacyNativeCheckpointGraphWriter) string {
	test.Helper()
	var state strings.Builder
	fmt.Fprintf(&state, "%t/%t/%d/%d/%d/%d/%d/%d/%#v", work.ready, work.verifying, work.depth, work.frameBytes,
		work.identity.nextID, work.identity.nodes, work.identity.owners, work.identity.views, work.result)
	if work.verify != nil {
		fmt.Fprintf(&state, "/verify=%d", work.verify.record.Identity)
	}
	for owner := work.identity.first; owner != nil; owner = owner.next {
		body, err := json.Marshal(owner.record)
		if err != nil {
			test.Fatal(err)
		}
		fmt.Fprintf(&state, "/owner=%s/%#v/%t", body, owner.body, owner.finished)
	}
	for frame := work.frame; frame != nil; frame = frame.parent {
		fmt.Fprintf(&state, "/frame=%d/%d/%d/%d/%#v/%#v/%#v", frame.phase, frame.position, frame.dirtyPosition,
			frame.markedCount, frame.state, frame.member, frame.result)
		if frame.cursor != nil {
			fmt.Fprintf(&state, "/cursor=%d/%d/%t/%t", frame.cursor.nextID, frame.cursor.remaining, frame.cursor.full, frame.cursor.finished)
		}
		if frame.members != nil {
			fmt.Fprintf(&state, "/members=%#v/%d", frame.members.values, frame.members.bytes)
		}
		if frame.dirty != nil {
			fmt.Fprintf(&state, "/dirty=%#v/%d", frame.dirty.values, frame.dirty.bytes)
		}
		if text, ok := frame.text.(*legacyNativeCheckpointTextWriter); ok {
			fmt.Fprintf(&state, "/text=%d/%d/%d/%#v/%t", text.phase, text.position, len(text.part), text.pending, text.ready)
		}
	}
	return state.String()
}

type legacyNativeGraphAdministrativeTrace struct {
	state    string
	boundary string
	reads    int
	writes   int
	bytes    int
}

func legacyNativeGraphAdministrativeCapture(test *testing.T, value legacyNativeAnyValue, expected []legacyNativeGraphAdministrativeTrace) (legacyNativeCheckpointValue, *legacyNativeGoGraphSetW85Store, []legacyNativeGraphAdministrativeTrace, []legacyNativeCheckpointRef, int) {
	test.Helper()
	storage := legacyNativeGoGraphSetW85Storage(test)
	var calls []legacyNativeCheckpointRef
	put := storage.io.Put
	storage.io.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		calls = append(calls, legacyNativeCheckpointRef{Hash: hash, Bytes: len(body)})
		return put(ctx, hash, body)
	}
	identity := newLegacyNativeCheckpointGraphIdentity()
	defer identity.close()
	work, err := newLegacyNativeCheckpointGraphWriter(context.Background(), value, storage.io, identity, legacyNativeGoGraphW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	var trace []legacyNativeGraphAdministrativeTrace
	consumed, maximum := 0, 0
	for steps := 1; steps <= legacyNativeGoGraphW85StepLimit; steps++ {
		boundary := legacyNativeGraphAdministrativeBoundary(work)
		var ready bool
		if expected == nil {
			ready, err = legacyNativeGoGraphSetW85Step[legacyNativeCheckpointValue](test, legacyNativeGraphAdministrativeOriginal{work}, storage)
		} else {
			ready, err = legacyNativeGoGraphSetW85Step[legacyNativeCheckpointValue](test, work, storage)
		}
		if err != nil {
			test.Fatal(err)
		}
		retained, err := work.retainedBytes()
		shared, sharedErr := identity.retainedBytes()
		if err != nil || sharedErr != nil || retained <= 0 || retained+shared > legacyNativeGoGraphW85Budget {
			test.Fatal("administrative transitions escaped aggregate admission", retained, shared, err, sharedErr)
		}
		entry := legacyNativeGraphAdministrativeTrace{state: legacyNativeGraphAdministrativeState(test, work), boundary: boundary,
			reads: storage.reads, writes: storage.writes, bytes: storage.transferred}
		trace = append(trace, entry)
		if expected != nil {
			start := consumed
			for consumed < len(expected) && consumed-start < 8 {
				previous := expected[consumed]
				consumed++
				if entry.state == previous.state {
					if entry.reads != previous.reads || entry.writes != previous.writes || entry.bytes != previous.bytes {
						test.Fatal("coalescing changed fresh IO at the original transition boundary")
					}
					break
				}
				if previous.boundary != "" {
					test.Fatal("step continued after payload, traversal, append, finish, CAS or verification", previous.boundary)
				}
			}
			if consumed == start || entry.state != expected[consumed-1].state {
				test.Fatal("step did not match at most eight original transitions", steps, consumed-start)
			}
			maximum = max(maximum, consumed-start)
		}
		if ready {
			if expected != nil && consumed != len(expected) {
				test.Fatal("coalescing omitted original transitions", consumed, len(expected))
			}
			result, err := work.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			legacyNativeGoGraphSetW85Released[legacyNativeCheckpointValue](test, work)
			return result, storage, trace, calls, maximum
		}
	}
	test.Fatal("administrative capture exceeded the existing finite fixture schedule")
	return legacyNativeCheckpointValue{}, storage, trace, calls, maximum
}

func TestLegacyNativeCheckpointGraphAdministrativeOracle(test *testing.T) {
	shared := map[string]any{"bool": true, "empty": map[string]any{}, "nil": map[string]any(nil)}
	wide := make([]any, 1025)
	for index := range wide {
		wide[index] = float64(index)
	}
	texts := make(map[string]any, 33)
	for index := range 33 {
		texts[fmt.Sprintf("key-%02d", index)] = strings.Repeat("x", 64)
	}
	backing := []any{true, "tail", shared, nil}
	boundaries := make(map[string]int)
	for _, fixture := range []struct {
		name   string
		source any
	}{
		{"typed-nil", map[string]any(nil)},
		{"empty-map", map[string]any{}},
		{"shared-owners", map[string]any{"first": shared, "alias": shared, "array": []any{nil, true, false, float64(3)}}},
		{"inline-pages", texts},
		{"array-pages", wide},
		{"full-backing", backing[:1:4]},
		{"text-65", strings.Repeat("x", 65)},
		{"text-64KiB", strings.Repeat("x", LegacyCanonicalEncodingBytes)},
		{"text-multichunk", strings.Repeat("x", LegacyCanonicalEncodingBytes+1)},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			registry := newLegacyNativeCheckpointGoGraphRegistry()
			defer registry.close()
			value, _ := legacyNativeGoGraphW85Import(test, fixture.source, registry)
			before, original, oldTrace, oldCalls, _ := legacyNativeGraphAdministrativeCapture(test, value, nil)
			after, candidate, newTrace, newCalls, maximum := legacyNativeGraphAdministrativeCapture(test, value, oldTrace)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(original.objects, candidate.objects) || !reflect.DeepEqual(oldCalls, newCalls) {
				test.Fatal("coalescing changed exact wire, references, ordered fresh writes or object bytes")
			}
			if len(newTrace) >= len(oldTrace) {
				test.Fatal("administrative coalescing did not reduce actual step count", len(oldTrace), len(newTrace))
			}
			for _, entry := range oldTrace {
				boundaries[entry.boundary]++
			}
			actual := legacyNativeCheckpointGraphInlineRestore(test, after, candidate)
			inspector := &legacyNativeGoGraphW85Inspector{}
			inspector.check(test, fixture.source, actual)
			test.Logf("original_steps=%d coalesced_steps=%d maximum_transitions=%d writes=%d bytes=%d", len(oldTrace), len(newTrace), maximum, original.writes, original.transferred)
		})
	}
	for _, boundary := range []string{"inline-or-leaf", "text", "cursor", "array", "tail", "append", "members-finish", "dirty-cursor", "dirty-finish", "owner", "verify"} {
		if boundaries[boundary] == 0 {
			test.Fatal("fixture omitted required real-work boundary", boundary)
		}
	}
}

func TestLegacyNativeCheckpointGraphAdministrativePerTransitionFences(test *testing.T) {
	for _, phase := range []string{"push", "wire"} {
		for _, scenario := range []string{"cancel", "budget", "owner", "identity-closed", "identity-failure"} {
			test.Run(phase+"/"+scenario, func(test *testing.T) {
				registry := newLegacyNativeCheckpointGoGraphRegistry()
				defer registry.close()
				child := any(true)
				if phase == "push" {
					child = map[string]any{"child": true}
				}
				value, _ := legacyNativeGoGraphW85Import(test, []any{child}, registry)
				storage := legacyNativeGoGraphSetW85Storage(test)
				identity := newLegacyNativeCheckpointGraphIdentity()
				defer identity.close()
				work, err := newLegacyNativeCheckpointGraphWriter(context.Background(), value, storage.io, identity, legacyNativeGoGraphW85Budget)
				if err != nil {
					test.Fatal(err)
				}
				defer work.close()
				for work.frame.phase != legacyNativeCheckpointGraphCaptureChild {
					if ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointValue](test, legacyNativeGraphAdministrativeOriginal{work}, storage); ready || err != nil {
						test.Fatal("fixture did not reach the original child-push cut", ready, err)
					}
				}
				parent := work.frame
				injected := false
				want := ErrIntegrity
				ctx := legacyNativeGoGraphSetTrivialBatchContext{Context: context.Background(), check: func() error {
					frame := work.frame
					atCut := frame != nil && frame != parent && frame.phase == legacyNativeCheckpointGraphCaptureMembers
					if phase == "wire" {
						atCut = frame == parent && frame.phase == legacyNativeCheckpointGraphCaptureAppend
					}
					if injected || !atCut {
						return nil
					}
					injected = true
					switch scenario {
					case "cancel":
						want = context.Canceled
						return want
					case "budget":
						work.budget, want = 0, ErrDocumentResourceLimit
					case "owner":
						if frame.captured.owner.object != nil {
							frame.captured.owner.object.revision++
						} else {
							frame.captured.owner.array.revision++
						}
						want = ErrConflict
					case "identity-closed":
						identity.closed = true
					case "identity-failure":
						identity.failure, want = ErrConflict, ErrConflict
					}
					return nil
				}}
				storage.current = ctx
				if ready, err := work.step(ctx); !injected || ready || !errors.Is(err, want) || storage.reads != 0 || storage.writes != 0 {
					test.Fatal("coalesced transition bypassed a fresh fence", injected, ready, err, want, storage.reads, storage.writes)
				}
				legacyNativeCheckpointStringsOracleFailed(test, work, want)
			})
		}
	}
}
