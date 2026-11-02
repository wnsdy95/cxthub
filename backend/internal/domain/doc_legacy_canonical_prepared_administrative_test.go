package domain

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestLegacyCanonicalPreparedAdministrativeBoundaries(test *testing.T) {
	for _, mode := range []string{"complete", "write-error", "cancel", "budget"} {
		test.Run(mode, func(test *testing.T) {
			native, encoding, _ := legacyCanonicalPreparedMachineFixture(test, "repeated-keys")
			defer encoding.release()
			for attempts := 0; ; attempts++ {
				body, complete, err := encoding.Next(context.Background())
				if err != nil || complete || attempts == 1<<17 {
					test.Fatal("fixture missed unfinished output", complete, err)
				}
				if len(body) > 0 && encoding.preparation == nil {
					break
				}
			}
			grant, err := native.object.mutation.beginCapture()
			if err != nil {
				test.Fatal(err)
			}
			defer grant.release()
			binding := HashContent([]byte("prepared-administrative"))
			retained, err := encoding.RetainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			var writers [2]*legacyCanonicalPreparedCheckpoint
			var stores [2]*legacyCanonicalPreparedValueTestStore
			var contexts [2]context.Context
			var calls [2][]legacyNativeCheckpointRef
			failure := errors.New("checkpoint write failed")
			for index := range writers {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				contexts[index] = ctx
				storage := legacyCanonicalPreparedValueTestStorage(test)
				stores[index] = storage
				put := storage.io.Put
				storage.io.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
					calls[index] = append(calls[index], legacyNativeCheckpointRef{Hash: hash, Bytes: len(body)})
					if !grant.valid(native.object.mutation) {
						test.Fatal("checkpoint lost source freeze")
					}
					if mode == "write-error" {
						return failure
					}
					if mode == "cancel" {
						cancel()
						return ctx.Err()
					}
					return put(ctx, hash, body)
				}
				writer, err := newLegacyCanonicalPreparedCheckpoint(ctx, encoding, binding, storage.io, legacyCanonicalPreparedValueTestBudget-retained)
				if err != nil {
					test.Fatal(err)
				}
				writers[index] = writer
				defer writer.close()
				if mode == "budget" {
					writer.budget = 1
				}
			}
			oldSteps, newSteps := 0, 0
			beforeInspect, beforeKeys := encoding.inspectWork, encoding.keyWork
			for range 1 << 18 {
				var ready [2]bool
				var failures [2]error
				for index, writer := range writers {
					stores[index].bytes = 0
					writer.attemptedIO = false
					if index == 0 {
						for range 8 {
							oldSteps++
							ready[index], failures[index] = writer.stepOnce(contexts[index])
							if ready[index] || failures[index] != nil || writer.attemptedIO || writer.emissionPending() {
								break
							}
						}
					} else {
						newSteps++
						ready[index], failures[index] = writer.step(contexts[index])
					}
					if stores[index].bytes > LegacyCanonicalEncodingBytes {
						test.Fatal("checkpoint exceeded existing IO quantum", stores[index].bytes)
					}
				}
				oldHeap, oldErr := writers[0].retainedBytes()
				newHeap, newErr := writers[1].retainedBytes()
				if ready[0] != ready[1] || !errors.Is(failures[0], failures[1]) || oldErr != newErr || oldHeap != newHeap ||
					writers[0].phase != writers[1].phase || writers[0].index != writers[1].index ||
					!reflect.DeepEqual(writers[0].state, writers[1].state) || !reflect.DeepEqual(calls[0], calls[1]) ||
					stores[0].reads != stores[1].reads || stores[0].bytes != stores[1].bytes {
					test.Fatal("batched step crossed an original IO, failure, heap or transition boundary", newSteps, failures)
				}
				if encoding.inspectWork != beforeInspect || encoding.keyWork != beforeKeys {
					test.Fatal("checkpoint advanced the frozen encoder")
				}
				if failures[0] != nil {
					want := map[string]error{"write-error": failure, "cancel": context.Canceled, "budget": ErrDocumentResourceLimit}[mode]
					if want == nil || !errors.Is(failures[0], want) {
						test.Fatal("unexpected failure", failures[0])
					}
					return
				}
				if ready[0] {
					if mode != "complete" || oldSteps <= newSteps || writers[0].ref != writers[1].ref || !reflect.DeepEqual(stores[0].objects, stores[1].objects) {
						test.Fatal("coalescing changed wire/root or failed to reduce steps", oldSteps, newSteps)
					}
					test.Logf("original_steps=%d batched_steps=%d writes=%d", oldSteps, newSteps, stores[1].writes)
					return
				}
			}
			test.Fatal("checkpoint exceeded its finite schedule")
		})
	}
}
