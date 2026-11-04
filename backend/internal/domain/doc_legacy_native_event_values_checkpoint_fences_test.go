package domain

import (
	"context"
	"errors"
	"testing"
)

func TestLegacyNativeEventValuesCheckpointMutationAndCancellation(test *testing.T) {
	for _, scenario := range []string{"cancel-active-child", "source-stop", "write-failure", "early-take", "budget", "list-shape", "wrong-ledger"} {
		test.Run(scenario, func(test *testing.T) {
			decoder, buffer := legacyDecoderCheckpointCut(test, legacyEventValuesCheckpointFixtures()[0])
			list, _ := legacyEventValuesCheckpointList(test, decoder)
			storage := legacyNativeAnyW78Store(test)
			budget := legacyDecoderCheckpointBudget
			if scenario == "budget" {
				budget = 1
			}
			if scenario == "list-shape" {
				list.paged = false
			}
			if scenario == "wrong-ledger" {
				list.values.payload.bytes--
			}
			work, err := newLegacyNativeEventValuesCheckpoint(context.Background(), list, HashContent(buffer.body), decoder.source.Bytes, storage.io(), budget)
			if scenario == "budget" || scenario == "list-shape" {
				if err == nil {
					work.close()
					test.Fatal("invalid capture was admitted")
				}
				legacyNormalizationCaptureRQ87Idle(test, decoder.mutation)
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "early-take" {
				if _, err := work.take(ctx); err == nil {
					test.Fatal("incomplete capture returned a root")
				}
			} else {
				if scenario == "source-stop" {
					decoder.mutation.stop()
				}
				if scenario == "write-failure" {
					storage.failPut = ErrConflict
				}
				var failure error
				for attempt := 0; attempt < legacyNativeAnyW78StepLimit; attempt++ {
					if scenario == "cancel-active-child" && work.fields != nil {
						cancel()
					}
					ready, err := work.step(ctx)
					if ready {
						test.Fatal("failed capture produced a complete root")
					}
					if err != nil {
						failure = err
						break
					}
				}
				if failure == nil {
					test.Fatal("failed capture never terminated")
				}
				if scenario == "cancel-active-child" && !errors.Is(failure, context.Canceled) {
					test.Fatal("cancellation lost its cause", failure)
				}
			}
			work.close()
			if decoder.mutation.activity.Load() != 0 || decoder.mutation.capture.Load() != nil {
				test.Fatal("capture did not release all nested grants")
			}
			if retained, err := work.retainedBytes(); err != nil || retained != 0 {
				test.Fatal("failed capture retained state", retained, err)
			}
		})
	}
}

func TestLegacyNativeEventValuesCheckpointMalformedStream(test *testing.T) {
	for _, scenario := range []string{"wrong-binding", "wrong-source", "schema", "visible-past-length", "nil-with-records", "wrong-kind", "truncated", "trailing", "cancel", "budget"} {
		test.Run(scenario, func(test *testing.T) {
			storage := legacyNativeAnyW78Store(test)
			ctx := context.Background()
			binding := HashContent([]byte("bounded-values-stream"))
			schema, err := legacyNativeEventRecordFieldSchema()
			if err != nil {
				test.Fatal(err)
			}
			header := legacyNativeEventValuesCheckpointEntry{Kind: legacyNativeEventValuesEntryList}
			switch scenario {
			case "visible-past-length":
				header.Count = 1
			case "nil-with-records":
				header.Nil, header.Length = true, 1
			case "wrong-kind":
				header.Kind = legacyNativeEventValuesEntryBlocks
			case "truncated":
				header.Length = 1
			}
			var rows legacyNativeCheckpointListWriter[legacyNativeEventValuesCheckpointEntry]
			defer rows.close()
			if err := rows.append(ctx, storage.io(), header); err != nil {
				test.Fatal(err)
			}
			if scenario == "trailing" {
				if err := rows.append(ctx, storage.io(), header); err != nil {
					test.Fatal(err)
				}
			}
			tree, err := rows.finish(ctx, storage.io())
			if err != nil {
				test.Fatal(err)
			}
			state := legacyNativeEventValuesCheckpointRoot{Version: 1, Schema: schema, Binding: binding, SourceBytes: 1024, Entries: tree}
			if scenario == "schema" {
				state.Schema = HashContent([]byte("foreign-model"))
			}
			root, err := legacyNativeCheckpointWriteJSON(ctx, storage.io(), state)
			if err != nil {
				test.Fatal(err)
			}
			budget, sourceBytes := legacyDecoderCheckpointBudget, 1024
			if scenario == "wrong-binding" {
				binding = HashContent([]byte("different-owner"))
			}
			if scenario == "wrong-source" {
				sourceBytes = 1025
			}
			if scenario == "budget" {
				budget = 1
			}
			restore, err := newLegacyNativeEventValuesCheckpointRestore(ctx, root, binding, sourceBytes, storage.io(), budget)
			if scenario == "budget" {
				if err == nil {
					restore.close()
					test.Fatal("under-budget restore allocated its state")
				}
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			call, cancel := context.WithCancel(ctx)
			defer cancel()
			if scenario == "cancel" {
				cancel()
			}
			var failure error
			for attempt := 0; attempt < legacyNativeAnyW78StepLimit; attempt++ {
				ready, err := restore.step(call)
				if ready {
					test.Fatal("malformed stream became a restored data root")
				}
				if err != nil {
					failure = err
					break
				}
			}
			if failure == nil {
				test.Fatal("malformed stream never terminated")
			}
			if _, err := restore.take(ctx); err == nil {
				test.Fatal("failed restore exposed partial data")
			}
			if retained, err := restore.retainedBytes(); err != nil || retained != 0 {
				test.Fatal("failed restore retained partial values", retained, err)
			}
		})
	}
}
