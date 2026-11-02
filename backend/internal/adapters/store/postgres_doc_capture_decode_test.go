//go:build postgres

package store

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestDocCaptureDecodePGYieldsBeforeWholeProof(test *testing.T) {
	for _, representation := range []string{"plain", "zstd"} {
		test.Run(representation, func(test *testing.T) {
			testDocCaptureDecodePGQuantum(test, representation == "zstd")
		})
	}
}

func testDocCaptureDecodePGQuantum(test *testing.T, compressed bool) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	stored := fixture.raw
	if compressed {
		stored = docCompress(stored)
		if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, stored); err != nil {
			test.Fatal(err)
		}
	}
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	peer, trace := docCaptureWorkPGStore(test, storage, ctx)
	work := docCaptureWorkPGPrepare(test, peer, ctx, job)
	docCaptureWorkPGCopy(test, work, ctx, job)
	queries := trace.bodyReads.Load()
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	progress, err := work.Step(ctx, job)
	if err != nil || progress != outbound.DocRangeWorkPending || work.work != nil {
		test.Fatal("first local decode consumed the whole document before yielding", progress, err)
	}
	resources, err := work.RetainedResources()
	if err != nil || resources.SpoolBytes != int64(len(stored)+domain.ConversationManifestChunkBytes) || resources.HeapBytes > 2<<20 || resources.KeepPermit != compressed {
		test.Fatal("first decode quantum did not retain only its independent partial output", resources, err)
	}
	if trace.bodyReads.Load() != queries || legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
		test.Fatal("local decode reread SQL body or changed persisted state")
	}
}

func TestDocCaptureDecodePGNextQuantumRechecksSourceAndJob(test *testing.T) {
	for _, change := range []string{"source version", "ownership", "lease expiry", "job fence", "snapshot identity", "source incarnation"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			peer, _ := docCaptureWorkPGStore(test, storage, ctx)
			work := docCaptureWorkPGPrepare(test, peer, ctx, job)
			docCaptureWorkPGCopy(test, work, ctx, job)
			if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.decoding == nil || work.decoding.decoded.size != domain.ConversationManifestChunkBytes {
				test.Fatal("decode did not stop after its first private output quantum", progress, err)
			}
			legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
			private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			progress, err := work.Step(ctx, job)
			want := domain.ErrConflict
			if change == "ownership" || change == "snapshot identity" {
				want = domain.ErrNotFound
			}
			if !errors.Is(err, want) || progress == outbound.DocRangeWorkPublishable || work.work != nil || work.decoding.decoded.size != domain.ConversationManifestChunkBytes {
				test.Fatal("revoked decode advanced or changed source/job error", change, progress, err)
			}
			if legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
				test.Fatal("failed decode changed persisted state")
			}
		})
	}
}
