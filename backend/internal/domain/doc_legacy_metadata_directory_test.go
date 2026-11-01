package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestLegacyRangeMetadataPreparationFinalizesDirectoriesInBoundedSteps(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, DocRangePageEvents*DocRangeDirectoryFanout+1)
	state, plan, source := legacyMetadataWorkFixture(test, proof)
	spec, err := plan.Spec()
	if err != nil {
		test.Fatal(err)
	}
	var actual []DocRangeObject
	save := func(_ context.Context, object DocRangeObject) error {
		actual = append(actual, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
		return nil
	}
	for step := 0; step < spec.Blocks; step++ {
		complete, err := state.Step(context.Background(), save)
		if err != nil || complete || len(source.reads) != step+1 {
			test.Fatal("source quantum scanned extra blocks or sealed directory work prematurely", step, complete, err)
		}
		legacyMetadataWorkPending(test, state)
	}
	for step := 0; step < 5; step++ {
		before, count := len(source.reads), len(actual)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		complete, err := state.Step(ctx, save)
		if complete || !errors.Is(err, context.Canceled) || len(source.reads) != before || len(actual) != count {
			test.Fatal("pre-canceled directory quantum changed prepared work", err)
		}
		complete, err = state.Step(context.Background(), save)
		if err != nil || len(source.reads) != before || len(actual) != count+1 || complete != (step == 4) || len(actual[count].Bytes) > MaxDocRangeDirectoryBytes {
			test.Fatal("directory quantum reread source, emitted multiple objects, or sealed early", step, complete, err)
		}
		if !complete {
			legacyMetadataWorkPending(test, state)
		}
	}
	metadata, err := state.Metadata()
	if err != nil {
		test.Fatal(err)
	}
	var expected []DocRangeObject
	want, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(_ context.Context, object DocRangeObject) error {
		expected = append(expected, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
		return nil
	})
	if err != nil || !reflect.DeepEqual(metadata, want) || !reflect.DeepEqual(actual, expected) {
		test.Fatal("bounded finalization changed the descriptor or ordered object bytes", err)
	}
}

func TestLegacyRangeMetadataPreparationDirectoryFailuresCannotSeal(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, 1)
	for _, fault := range []string{"event-error", "chunk-error", "root-error", "event-cancel", "chunk-cancel", "root-cancel"} {
		test.Run(fault, func(test *testing.T) {
			state, plan, source := legacyMetadataWorkFixture(test, proof)
			spec, _ := plan.Spec()
			for step := 0; step < spec.Blocks; step++ {
				if complete, err := state.Step(context.Background(), func(context.Context, DocRangeObject) error { return nil }); complete || err != nil {
					test.Fatal("source phase failed or sealed early", complete, err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("directory persistence failed")
			for step, stage := range []string{"event", "chunk", "root"} {
				before := len(source.reads)
				complete, err := state.Step(ctx, func(context.Context, DocRangeObject) error {
					if fault == stage+"-error" {
						return failure
					}
					if fault == stage+"-cancel" {
						cancel()
					}
					return nil
				})
				if len(source.reads) != before || complete {
					test.Fatal("directory failure reread source or exposed a receipt", step, err)
				}
				if fault != stage+"-error" && fault != stage+"-cancel" {
					if err != nil {
						test.Fatal(err)
					}
					legacyMetadataWorkPending(test, state)
					continue
				}
				if !errors.Is(err, failure) && !errors.Is(err, context.Canceled) {
					test.Fatal("directory failure was lost", err)
				}
				if metadata, err := state.Metadata(); !errors.Is(err, ErrIntegrity) || metadata.Valid() {
					test.Fatal("failed directory exposed metadata", err)
				}
				if complete, err := state.Step(context.Background(), func(context.Context, DocRangeObject) error {
					test.Fatal("failed metadata resumed directory callbacks")
					return nil
				}); complete || !errors.Is(err, ErrIntegrity) {
					test.Fatal("failed metadata was revived", complete, err)
				}
				return
			}
			test.Fatal("directory fault was not reached")
		})
	}
}
