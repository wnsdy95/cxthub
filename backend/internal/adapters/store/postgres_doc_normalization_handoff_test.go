//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func docNormalizationPGConsumedInput(test *testing.T, work *preparedLegacyCaptureWork, ctx context.Context, job domain.DocRangeIndexJob) string {
	test.Helper()
	if work.source != nil || work.decoding == nil || !work.decoding.complete || work.normalization == nil {
		test.Fatal("fixture did not enter the real normalization path")
	}
	decoder := work.decoding
	path := decoder.decoded.file.Name()
	for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit; quantum++ {
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
			test.Fatal("normalization did not reach its consumed-input boundary", quantum, progress, err)
		}
		if work.decoding == nil {
			if !decoder.closed || work.normalization == nil || work.normalization.canonical != nil || work.canonical != nil || work.canonicalProof.Valid() || work.work != nil {
				test.Fatal("input was released after proof or replaced with a fake decoder")
			}
			consumed, err := work.normalization.inputConsumed()
			if err != nil || !consumed {
				test.Fatal("worker released input before the actual domain consumption boundary", consumed, err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				test.Fatal("consumed raw-input spool is still retained", err)
			}
			return path
		}
	}
	test.Fatal("worker retained original input through all normalization phases")
	return ""
}

func TestDocNormalizationWorkPGConsumedInputContinuesToPublication(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5*(64<<10))
	work, job, trace := docNormalizationPGStart(test, storage, ctx, fixture)
	defer work.Close()
	queries := trace.bodyReads.Load()
	path := docNormalizationPGConsumedInput(test, work, ctx, job)
	for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit; quantum++ {
		if err := work.matches(job); err != nil {
			test.Fatal("legitimate normalization/proof owner was classified as closed", err)
		}
		resources, err := work.RetainedResources()
		if err != nil || resources.HeapBytes <= 0 || resources.SpoolBytes < 0 || work.decoding != nil || work.source != nil {
			test.Fatal("consumed input was recreated or lost its actual resource accounting", resources, err)
		}
		progress, err := work.Step(ctx, job)
		if err != nil || progress == outbound.DocRangeWorkCompatibilityRequired {
			test.Fatal("real downstream normalization/proof/publication preparation failed", quantum, progress, err)
		}
		if trace.bodyReads.Load() != queries {
			test.Fatal("normalization/proof replayed the original SQL payload after input release")
		}
		if progress != outbound.DocRangeWorkPublishable {
			continue
		}
		if err := work.Publish(ctx, job); err != nil {
			test.Fatal("real worker could not publish without a retained original decoder", err)
		}
		proof, err := storage.ReadVerifiedDoc(ctx, fixture.repo, fixture.hash)
		if err != nil || !bytes.Equal(proof.Bytes(), fixture.raw) {
			test.Fatal("actual publication changed canonical content", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			test.Fatal("consumed input spool was resurrected", err)
		}
		return
	}
	test.Fatal("downstream worker did not publish in the original finite schedule")
}

func TestDocNormalizationWorkPGConsumedInputRetainsFences(test *testing.T) {
	for _, change := range []string{"source version", "ownership", "lease expiry", "cancellation"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
			work, job, trace := docNormalizationPGStart(test, storage, ctx, fixture)
			defer work.Close()
			docNormalizationPGConsumedInput(test, work, ctx, job)
			before := work.normalization
			queries := trace.bodyReads.Load()
			selected, cancel := context.WithCancel(ctx)
			defer cancel()
			want := domain.ErrConflict
			if change == "cancellation" {
				cancel()
				want = context.Canceled
			} else {
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
				if change == "ownership" {
					want = domain.ErrNotFound
				}
			}
			progress, err := work.Step(selected, job)
			if !errors.Is(err, want) || progress != 0 || work.decoding != nil || work.normalization != before || work.canonicalProof.Valid() || work.work != nil || trace.bodyReads.Load() != queries {
				test.Fatal("consumed-input worker bypassed its current source/ownership/lease fence", progress, err)
			}
		})
	}
}
