//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestDocCanonicalSourceWorkPGDoesNotMaterializeWholeProof(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	peer, trace := docCaptureWorkPGStore(test, storage, ctx)
	work := docCaptureWorkPGPrepare(test, peer, ctx, job)
	docCaptureWorkPGCopy(test, work, ctx, job)
	docCaptureWorkPGDecode(test, work, ctx, job)
	queries := trace.bodyReads.Load()
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	for quantum := range 3 {
		progress, err := work.Step(ctx, job)
		if err != nil || progress != outbound.DocRangeWorkPending || work.work != nil {
			test.Fatal("post-decode verification consumed the whole source before yielding", quantum, progress, err)
		}
		resources, err := work.RetainedResources()
		if err != nil || resources.HeapBytes >= 2<<20 || resources.SpoolBytes != int64(len(fixture.raw)) {
			test.Fatal("canonical verification retained whole-body proof or unused capture", quantum, resources, err)
		}
		if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			test.Fatal("partial source proof became publishable", err)
		}
	}
	if trace.bodyReads.Load() != queries || legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
		test.Fatal("source proof reread SQL payload or changed persisted state")
	}
}

func TestDocCanonicalSourceWorkPGExtraOutputCannotSealAuthority(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	peer, _ := docCaptureWorkPGStore(test, storage, ctx)
	work := docCaptureWorkPGPrepare(test, peer, ctx, job)
	docCaptureWorkPGCopy(test, work, ctx, job)
	docCaptureWorkPGDecode(test, work, ctx, job)
	limit := 6*maxLegacyReadBytes/domain.ConversationManifestChunkBytes + 8
	for quantum := 0; quantum < limit && (work.proofData == nil || work.proofData.size != len(fixture.stream)); quantum++ {
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil || work.canonicalProof.Valid() {
			test.Fatal("source proof did not stop at its final output write", progress, err)
		}
	}
	if work.proofData == nil || work.proofData.size != len(fixture.stream) {
		test.Fatal("source proof did not reach complete private output")
	}
	blocks := (len(fixture.stream) + domain.ConversationManifestChunkBytes - 1) / domain.ConversationManifestChunkBytes
	for range blocks {
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil || work.canonicalProof.Valid() {
			test.Fatal("readback exposed authority before exact EOF", progress, err)
		}
	}
	if _, err := work.proofData.file.WriteAt([]byte("!"), int64(work.proofData.size)); err != nil {
		test.Fatal(err)
	}
	progress, err := work.Step(ctx, job)
	if !errors.Is(err, domain.ErrPreparedDocRangeObject) || progress == outbound.DocRangeWorkPublishable || work.canonicalProof.Valid() || work.work != nil {
		test.Fatal("physical trailing output minted sealed source authority", progress, err, work.canonicalProof.Valid())
	}
}

func TestDocCanonicalSourceWorkPGHandoffExtentDamageIsRetryable(test *testing.T) {
	for _, damage := range []string{"truncate", "extra"} {
		for _, changed := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/changed=%t", damage, changed), func(test *testing.T) {
				storage, ctx := chunkReusePG(test)
				fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
				job := legacyByteStagePGClaim(test, storage, ctx, fixture)
				peer, _ := docCaptureWorkPGStore(test, storage, ctx)
				work := docCaptureWorkPGPrepare(test, peer, ctx, job)
				docCaptureWorkPGBeforeHandoff(test, work, ctx, job)
				if !work.canonicalProof.Valid() || work.proofData == nil {
					test.Fatal("fixture did not seal its canonical source before handoff")
				}
				if damage == "truncate" {
					if err := work.proofData.file.Truncate(int64(work.proofData.size - 1)); err != nil {
						test.Fatal(err)
					}
				} else if _, err := work.proofData.file.WriteAt([]byte("!"), int64(work.proofData.size)); err != nil {
					test.Fatal(err)
				}
				if changed {
					legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, "source version")
				}
				private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
				public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
				progress, err := work.Step(ctx, job)
				want := domain.ErrPreparedDocRangeObject
				if changed {
					want = domain.ErrConflict
				}
				if !errors.Is(err, want) || progress == outbound.DocRangeWorkPublishable || work.work != nil {
					test.Fatal("handoff damaged private output was rejected as source data or delegated", progress, err, want)
				}
				if legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
					test.Fatal("damaged handoff modified stored/public state")
				}
			})
		}
	}
}

func TestDocCanonicalSourceWorkPGEveryQuantumFencesAuthority(test *testing.T) {
	for _, phase := range []string{"hash", "copy", "readback"} {
		for _, change := range []string{"source version", "ownership", "lease expiry", "job fence", "snapshot identity", "source incarnation", "cancellation"} {
			test.Run(phase+"/"+change, func(test *testing.T) {
				storage, ctx := chunkReusePG(test)
				fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
				job := legacyByteStagePGClaim(test, storage, ctx, fixture)
				peer, trace := docCaptureWorkPGStore(test, storage, ctx)
				work := docCaptureWorkPGPrepare(test, peer, ctx, job)
				docCaptureWorkPGCopy(test, work, ctx, job)
				docCaptureWorkPGDecode(test, work, ctx, job)
				for step := 0; step < docCaptureWorkPGPreparationStepLimit; step++ {
					ready := work.canonical != nil && work.proofData != nil
					if ready && (phase == "hash" || phase == "copy" && work.proofData.size > 0 || phase == "readback" && work.proofData.size == len(fixture.stream)) {
						break
					}
					if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil || work.canonicalProof.Valid() {
						test.Fatal("canonical fixture did not stop within its private quantum phase", phase, progress, err)
					}
				}
				if phase == "readback" {
					if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.canonicalProof.Valid() {
						test.Fatal("fixture did not retain a partial readback", progress, err)
					}
				}
				if work.canonical == nil || work.proofData == nil || work.canonicalProof.Valid() {
					test.Fatal("fixture missed the requested proof phase", phase)
				}
				selected, cancel := context.WithCancel(ctx)
				defer cancel()
				if change == "cancellation" {
					cancel()
				} else {
					legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
				}
				state := fmt.Sprintf("%#v", work.canonical)
				outputBytes := work.proofData.size
				private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
				public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
				queries := trace.queries.Load()
				progress, err := work.Step(selected, job)
				want := domain.ErrConflict
				if change == "ownership" || change == "snapshot identity" {
					want = domain.ErrNotFound
				} else if change == "cancellation" {
					want = context.Canceled
				}
				if !errors.Is(err, want) || progress == outbound.DocRangeWorkPublishable || work.work != nil || work.canonicalProof.Valid() || work.proofData.size != outputBytes || fmt.Sprintf("%#v", work.canonical) != state {
					test.Fatal("revoked canonical quantum changed private progress or error", phase, change, progress, err)
				}
				if change == "cancellation" && trace.queries.Load() != queries {
					test.Fatal("canceled canonical quantum issued SQL")
				}
				if legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
					test.Fatal("revoked canonical quantum modified stored/public state")
				}
			})
		}
	}
}

func TestDocCanonicalSourceWorkPGDamageDuringCandidateConstructionIsRetryable(test *testing.T) {
	for _, damage := range []string{"truncate", "extra"} {
		for _, changed := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/changed=%t", damage, changed), func(test *testing.T) {
				storage, ctx := chunkReusePG(test)
				fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
				job := legacyByteStagePGClaim(test, storage, ctx, fixture)
				peer, _ := docCaptureWorkPGStore(test, storage, ctx)
				work := docCaptureWorkPGPrepare(test, peer, ctx, job)
				docCaptureWorkPGBeforeHandoff(test, work, ctx, job)
				base, cancel := context.WithCancel(ctx)
				paused := &docCaptureConfirmationContext{Context: base, entered: make(chan struct{}), resume: make(chan struct{}), target: "store.initializeLegacyByteWork"}
				results, finished := make(chan error, 1), make(chan struct{})
				test.Cleanup(func() {
					cancel()
					paused.unblock()
					<-finished
				})
				go func() {
					defer close(finished)
					_, err := work.Step(paused, job)
					results <- err
				}()
				select {
				case <-paused.entered:
				case <-finished:
					test.Fatal("candidate did not pause after its first physical extent check", <-results)
				case <-ctx.Done():
					test.Fatal(ctx.Err())
				}
				if damage == "truncate" {
					if err := work.proofData.file.Truncate(int64(work.proofData.size - 1)); err != nil {
						test.Fatal(err)
					}
				} else if _, err := work.proofData.file.WriteAt([]byte("!"), int64(work.proofData.size)); err != nil {
					test.Fatal(err)
				}
				if changed {
					legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, "source version")
				}
				paused.unblock()
				err := <-results
				want := domain.ErrPreparedDocRangeObject
				if changed {
					want = domain.ErrConflict
				}
				if !errors.Is(err, want) || work.work != nil {
					test.Fatal("late candidate extent damage was rejected as source data or delegated", err, want)
				}
			})
		}
	}
}
