//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docNativeInspectPGContext struct {
	context.Context
	inspected atomic.Bool
}

func (ctx *docNativeInspectPGContext) Err() error {
	if !ctx.inspected.Load() {
		var callers [4]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		preparation, _ := frames.Next()
		consumer, _ := frames.Next()
		if strings.HasSuffix(preparation.Function, "domain.(*legacyCanonicalPreparation).step") &&
			strings.HasSuffix(consumer.Function, "domain.(*LegacyCanonicalEncoding).Next") {
			ctx.inspected.Store(true)
		}
	}
	return ctx.Context.Err()
}

func TestDocNormalizationWorkPGNativeInspectPendingRetainsFences(test *testing.T) {
	for _, change := range []string{"unchanged", "source version", "ownership", "lease expiry", "job fence", "snapshot identity", "source incarnation", "cancellation"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
			work, job, trace := docNormalizationPGStart(test, storage, ctx, fixture)
			inspection := &docNativeInspectPGContext{Context: ctx}
			bodyReads := trace.bodyReads.Load()
			private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			var extents [domain.LegacyNormalizationSlots]int
			for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit; quantum++ {
				if work.normalization == nil || work.normalization.storage == nil || work.normalization.canonical != nil {
					test.Fatal("normalization passed native inspection without observing its Next entry")
				}
				for slot, spool := range work.normalization.storage.spools {
					extents[slot] = spool.size
				}
				if progress, err := work.Step(inspection, job); err != nil || progress != outbound.DocRangeWorkPending ||
					work.work != nil || work.canonicalProof.Valid() || work.original {
					test.Fatal("native inspection did not remain provisional", quantum, progress, err)
				}
				if inspection.inspected.Load() {
					break
				}
			}
			if !inspection.inspected.Load() {
				test.Fatal("actual incremental native inspection was not observed within the existing preparation allowance")
			}
			if work.normalization == nil || work.normalization.storage == nil || work.normalization.canonical != nil {
				test.Fatal("native inspect quantum acquired a canonical proof phase")
			}
			for slot, spool := range work.normalization.storage.spools {
				if spool.size < extents[slot] || spool.size-extents[slot] > domain.ConversationManifestChunkBytes {
					test.Fatal("native inspect quantum exceeded bounded disk scratch progress", slot, spool.size, extents[slot])
				}
				extents[slot] = spool.size
			}
			if trace.bodyReads.Load() != bodyReads || private != legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) ||
				public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) {
				test.Fatal("native inspect pending reread SQL payload or changed persisted state")
			}
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("native inspect pending became publishable", err)
			}
			if change == "unchanged" {
				progress, err := docCaptureWorkPGDrainPreparation(work, ctx, job)
				if err != nil || progress != outbound.DocRangeWorkPending || work.work == nil || trace.bodyReads.Load() != bodyReads {
					test.Fatal("native inspect pending did not reach its verified same-process handoff", progress, err)
				}
				for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && progress != outbound.DocRangeWorkPublishable; quantum++ {
					progress, err = work.Step(ctx, job)
					if err != nil {
						test.Fatal(err)
					}
				}
				if progress != outbound.DocRangeWorkPublishable {
					test.Fatal("native inspect pending never became publishable")
				}
				if err := work.Publish(ctx, job); err != nil {
					test.Fatal(err)
				}
				proof, err := storage.ReadVerifiedDoc(ctx, fixture.repo, fixture.hash)
				if err != nil || !bytes.Equal(proof.Bytes(), fixture.raw) {
					test.Fatal("native inspect pending changed the original published canonical source", err)
				}
				return
			}
			selected, cancel := context.WithCancel(ctx)
			defer cancel()
			fenced := &docNativeInspectPGContext{Context: selected}
			want := error(domain.ErrConflict)
			if change == "cancellation" {
				cancel()
				want = context.Canceled
			} else {
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
				if change == "ownership" || change == "snapshot identity" {
					want = domain.ErrNotFound
				}
			}
			private = legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			public = legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			queries := trace.queries.Load()
			progress, err := work.Step(fenced, job)
			if !errors.Is(err, want) || progress != 0 || fenced.inspected.Load() || work.work != nil || work.canonicalProof.Valid() ||
				work.normalization == nil || work.normalization.storage == nil || work.normalization.canonical != nil {
				test.Fatal("fenced native pending resumed inspection or acquired authority", progress, err)
			}
			for slot, spool := range work.normalization.storage.spools {
				if spool.size != extents[slot] {
					test.Fatal("fenced native pending advanced scratch state", slot, spool.size, extents[slot])
				}
			}
			if trace.bodyReads.Load() != bodyReads || change == "cancellation" && trace.queries.Load() != queries {
				test.Fatal("fenced native pending reread payload or canceled work touched SQL")
			}
			if private != legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) || public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) {
				test.Fatal("fenced native pending changed persisted state")
			}
		})
	}
}
