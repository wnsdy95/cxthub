//go:build postgres

package store

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestDocMetadataQuantumPGPublicWorkDefersMetadataUntilStepping(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture, _ := docRangeWorkEmissionPGSource(test, storage, ctx, domain.DocRangePageEvents*domain.DocRangeDirectoryFanout+1)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	prepared := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, prepared)
	before, err := prepared.RetainedResources()
	if err != nil || before.SpoolBytes != int64(len(fixture.stream)) {
		test.Fatal("public proof handoff did not retain exactly its verified event stream without metadata", before, len(fixture.stream), err)
	}
	if err := prepared.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("public work published before its first quantum", err)
	}
	quanta := (len(fixture.stream) + domain.ConversationManifestChunkBytes - 1) / domain.ConversationManifestChunkBytes
	if quanta < 3 {
		test.Fatal("many-small-event fixture must cross several source quanta", quanta)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("public canonical proof copied private bytes before metadata preparation")
	}
	spooled := docRangeWorkPGResources(test, prepared, len(fixture.stream))
	if spooled.SpoolBytes != int64(len(fixture.stream)) || prepared.preparation == nil || prepared.preparation.ObjectCount() != 0 {
		test.Fatal("source-backed proof handoff emitted metadata instead of preparing its first source scan", spooled)
	}
	for step := 1; step <= quanta; step++ {
		complete, err := prepared.Step(ctx, job)
		if err != nil || complete != outbound.DocRangeWorkPending {
			test.Fatal("public work did not consume one metadata source quantum", step, quanta, complete, err)
		}
		resources, err := prepared.RetainedResources()
		if err != nil || resources.HeapBytes >= 16<<20 || resources.SpoolBytes <= spooled.SpoolBytes || resources.SpoolBytes >= 64<<20 {
			test.Fatal("small-event metadata did not remain bounded and incrementally spooled", step, resources, err)
		}
		if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
			test.Fatal("private quantum advertised public readiness", step, ready, err)
		}
	}
	for step := 1; step <= 5; step++ {
		if err := prepared.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			test.Fatal("public work sealed before the last directory quantum", step, err)
		}
		complete, err := prepared.Step(ctx, job)
		if err != nil || (complete == outbound.DocRangeWorkPublishable) != (step == 5) {
			test.Fatal("public work did not complete its five ordered directory objects", step, complete, err)
		}
		resources, err := prepared.RetainedResources()
		if err != nil || resources.HeapBytes >= 16<<20 || resources.SpoolBytes >= 64<<20 {
			test.Fatal("directory metadata exceeded existing retained-resource limits", step, resources, err)
		}
		if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
			test.Fatal("private directory quantum advertised public readiness", step, ready, err)
		}
	}
	if err := prepared.Publish(ctx, job); err != nil {
		test.Fatal("sealed public work could not publish", err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
	test.Logf("events=%d source_bytes=%d canonical_copy_verify_quanta=%d source_quanta=%d directory_quanta=5 initial_spool_bytes=%d initial_heap=%d", domain.DocRangePageEvents*domain.DocRangeDirectoryFanout+1,
		len(fixture.stream), 2*quanta, quanta, before.SpoolBytes, before.HeapBytes)
}
