package domain

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type legacyEventValuesPostIOOriginal struct {
	*legacyNativeEventValuesCheckpoint
}

func (original legacyEventValuesPostIOOriginal) step(ctx context.Context) (bool, error) {
	return original.stepRecord(ctx, true)
}

type legacyCanonicalSourcePostIOOriginal struct {
	*legacyCanonicalSourceCheckpoint
}

func (original legacyCanonicalSourcePostIOOriginal) step(ctx context.Context) (bool, error) {
	return original.stepOnce(ctx)
}

func TestLegacyNativeEventValuesParserPostIOPreservesBoundaries(test *testing.T) {
	legacyEventValuesPostIOPreservesBoundaries(test, false)
}

func TestLegacyCanonicalSourcePostIOPreservesBoundaries(test *testing.T) {
	legacyEventValuesPostIOPreservesBoundaries(test, true)
}

func legacyEventValuesPostIOPreservesBoundaries(test *testing.T, canonical bool) {
	test.Helper()
	child := `{"kind":"message","role":"assistant","blocks":[{"type":"text","text":"` + strings.Repeat("x", 64) + `"}]}`
	event := `{"kind":"compaction","replacement":[` + strings.Repeat(child+",", 255) + child + `],"replacement_complete":true}`
	stage, buffer := legacyCanonicalInitialCheckpointStage(test, event)
	list := &legacyNativeEventList{mutation: stage.mutation, paged: true, values: stage.pending[0].Encoding.nativeOwner.replacement}
	binding := HashContent(buffer.body)
	var expected []legacyEventValuesAdministrativeTrace
	var originalRoot legacyNativeCheckpointRef
	var originalObjects map[ContentHash][]byte
	var originalCalls []legacyNativeCheckpointRef
	for _, coalesced := range []bool{false, true} {
		storage := legacyNativeGoGraphSetW85Storage(test)
		var calls []legacyNativeCheckpointRef
		put := storage.io.Put
		storage.io.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
			calls = append(calls, legacyNativeCheckpointRef{Hash: hash, Bytes: len(body)})
			return put(ctx, hash, body)
		}
		var work legacyNativeGoGraphSetW85Work[legacyNativeCheckpointRef]
		var values *legacyNativeEventValuesCheckpoint
		var source *legacyCanonicalSourceCheckpoint
		var grant *legacyNormalizationCapture
		var err error
		if canonical {
			grant, err = stage.mutation.beginCapture()
			if err != nil {
				test.Fatal(err)
			}
			defer func() {
				if grant != nil {
					grant.release()
				}
			}()
			source, err = newLegacyCanonicalSourceCheckpoint(context.Background(), stage.pending[0].Encoding, stage.mutation,
				binding, len(buffer.body), storage.io, legacyDecoderCheckpointBudget)
			if err == nil {
				values, work = source.child, source
				if !coalesced {
					work = legacyCanonicalSourcePostIOOriginal{source}
				}
			}
		} else {
			values, err = newLegacyNativeEventValuesCheckpoint(context.Background(), list, binding, len(buffer.body), storage.io, legacyDecoderCheckpointBudget)
			work = legacyNativeEventScalarPairParser{values}
			if !coalesced {
				work = legacyEventValuesPostIOOriginal{values}
			}
		}
		if err != nil {
			test.Fatal(err)
		}
		defer work.close()
		consumed, steps, crossedIO := 0, 0, 0
		for ; steps < legacyNativeAnyW78StepLimit; steps++ {
			ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, work, storage)
			if err != nil {
				test.Fatal(err)
			}
			retained, err := work.retainedBytes()
			if err != nil || retained <= 0 || retained > legacyDecoderCheckpointBudget {
				test.Fatal("post-IO transitions escaped memory admission", retained, err)
			}
			stop := canonical && values.current != nil && values.current.phase == 7
			if source != nil {
				stop = stop || source.sourceGraph() != nil
			}
			state := legacyEventValuesAdministrativeTrace{state: legacyEventValuesAdministrativeState(test, values),
				stop: stop, reads: storage.reads, writes: storage.writes,
				bytes: storage.transferred, retained: retained}
			if !coalesced {
				expected = append(expected, state)
			} else {
				start := consumed
				var previous legacyEventValuesAdministrativeTrace
				if start > 0 {
					previous = expected[start-1]
				}
				attemptedIO := false
				for consumed < len(expected) && consumed-start < 8 {
					next := expected[consumed]
					changedIO := next.reads != previous.reads || next.writes != previous.writes || next.bytes != previous.bytes
					if attemptedIO && changedIO {
						test.Fatal("post-IO coalescing attempted a second original I/O group")
					}
					if attemptedIO {
						crossedIO++
					}
					attemptedIO = attemptedIO || changedIO
					consumed++
					if state.state == next.state {
						if state != next {
							test.Fatal("post-IO boundary changed bytes, retained memory, or current state")
						}
						break
					}
					if next.stop {
						test.Fatal("post-IO coalescing crossed a complete-record handoff")
					}
					previous = next
				}
				if consumed == start || state.state != expected[consumed-1].state {
					test.Fatal("post-IO coalescing exceeded eight original transitions")
				}
			}
			if ready {
				root, err := work.take(context.Background())
				if err != nil {
					test.Fatal(err)
				}
				if !coalesced {
					originalRoot, originalObjects, originalCalls = root, storage.objects, calls
				} else if root != originalRoot || !reflect.DeepEqual(storage.objects, originalObjects) ||
					!reflect.DeepEqual(calls, originalCalls) || consumed != len(expected) || crossedIO == 0 || steps >= len(expected) || !canonical && steps >= 400 {
					test.Fatal("post-IO coalescing lost exact wire or bounded progress", steps, consumed, crossedIO)
				}
				test.Logf("coalesced=%t steps=%d post_io_transitions=%d puts=%d", coalesced, steps+1, crossedIO, storage.writes)
				break
			}
		}
		if steps == legacyNativeAnyW78StepLimit {
			test.Fatal("post-IO checkpoint did not finish")
		}
		if grant != nil {
			grant.release()
			grant = nil
		}
		legacyNormalizationCaptureRQ87Idle(test, list.mutation)
	}
}
