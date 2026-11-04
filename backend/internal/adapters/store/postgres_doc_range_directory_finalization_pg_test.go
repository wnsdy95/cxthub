//go:build postgres

package store

import (
	"bytes"
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestDocRangeWorkPGDirectoryFinalizationStaysPrivateAndPreservesOrderedObjects(tests *testing.T) {
	for _, fixtureCase := range []struct {
		name           string
		events         int
		directorySteps int
	}{
		{name: "single-event-directory", events: domain.DocRangePageEvents, directorySteps: 3},
		{name: "multi-level-event-directory", events: domain.DocRangePageEvents*domain.DocRangeDirectoryFanout + 1, directorySteps: 5},
	} {
		tests.Run(fixtureCase.name, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			rangeSpoolTempDir(test)
			storage.ConfigureLegacyDerivedByteRangePublication(true)
			fixture, proof := docRangeWorkEmissionPGSource(test, storage, ctx, fixtureCase.events)
			generation, objects, err := domain.BuildLegacyDerivedDocByteRangeGeneration(ctx, proof)
			if err != nil {
				test.Fatal(err)
			}
			if steps := len(objects) - generation.EventPages.Count; steps != fixtureCase.directorySteps {
				test.Fatal("fixture changed its original ordered directory object count", steps, fixtureCase.directorySteps)
			}
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			original := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
			private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			work := docRangeWorkPGPrepare(test, storage, ctx, job)
			defer docRangeWorkPGClose(test, work)
			if legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private ||
				legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
				test.Fatal("source-backed canonical preparation changed private byte progress or public metadata")
			}
			sourceQuanta := (len(fixture.stream) + domain.ConversationManifestChunkBytes - 1) / domain.ConversationManifestChunkBytes
			for step := 1; step <= sourceQuanta; step++ {
				if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
					test.Fatal("source quantum sealed metadata before directory finalization", step, sourceQuanta, complete, err)
				}
				docRangeWorkPGAssertMetadataPending(test, work, ctx, job)
				docRangeWorkPGResources(test, work, len(fixture.stream))
			}
			if count := work.preparation.ObjectCount(); count != generation.EventPages.Count {
				test.Fatal("final source quantum did not flush exactly the original event pages", count, generation.EventPages.Count)
			}
			stage := work.session.stage
			stage.next, stage.complete = stage.blocks, true
			legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
			private = legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			repair := docRangeWorkPGCorruptByte(test, work.data.file, 0)
			defer repair()
			for step := 1; step <= fixtureCase.directorySteps; step++ {
				docRangeWorkPGAssertMetadataPending(test, work, ctx, job)
				before := work.preparation.ObjectCount()
				complete, err := work.Step(ctx, job)
				if err != nil || (complete == outbound.DocRangeWorkPublishable) != (step == fixtureCase.directorySteps) {
					test.Fatal("directory step reread source bytes or completed outside the final chunk root", step, complete, err)
				}
				after := work.metadata.ObjectCount()
				if work.preparation != nil {
					after = work.preparation.ObjectCount()
				}
				if after-before != 1 || after != generation.EventPages.Count+step {
					test.Fatal("directory step did not emit exactly its next single object", step, before, after)
				}
				if work.metadata.Valid() != (complete == outbound.DocRangeWorkPublishable) || (work.preparation == nil) != (complete == outbound.DocRangeWorkPublishable) {
					test.Fatal("metadata sealed before the final chunk root", step, complete)
				}
				if complete == outbound.DocRangeWorkPending {
					docRangeWorkPGAssertMetadataPending(test, work, ctx, job)
				}
				docRangeWorkPGResources(test, work, len(fixture.stream))
				body, err := work.objects.Read(ctx, before)
				if err != nil || !bytes.Equal(body, objects[before].Bytes) || domain.HashContent(body) != objects[before].Hash {
					test.Fatal("directory step changed the original ordered object or fingerprint", step, before, err)
				}
				if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
					test.Fatal("directory finalization advertised readiness before explicit publication", step, ready, err)
				}
				if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
					test.Fatal("directory finalization changed the source, ownership, snapshot, job, or public metadata", step)
				}
				if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
					test.Fatal("directory finalization rewrote complete private byte progress", step)
				}
			}
			repair()
			if err := work.Publish(ctx, job); err != nil {
				test.Fatal("fully sealed metadata could not publish", err)
			}
			legacyDerivedPublishedPG(test, storage, ctx, fixture)
			docPayloadProjectionPGAssertMetadata(test, storage, ctx, fixture, proof, work)
			if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
				reader, err := storage.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
				if err != nil {
					return err
				}
				var stream []byte
				for ordinal := 0; ordinal < generation.ChunkCount(); ordinal++ {
					want, descriptor := legacyByteStagePGBlock(fixture, ordinal)
					body, err := storage.ReadLegacyDerivedByteChunk(read, fixture.repo, fixture.hash, reader.Binding(), ordinal, descriptor)
					if err != nil {
						return err
					}
					if !bytes.Equal(body, want) {
						test.Fatal("directory finalization changed an original canonical byte range", ordinal)
					}
					stream = append(stream, body...)
				}
				canonical := domain.AssembleCanonicalDoc(fixture.envelope, [][]byte{stream})
				if !bytes.Equal(canonical, fixture.raw) || domain.HashContent(canonical) != fixture.hash {
					test.Fatal("directory finalization changed the original canonical document or identity")
				}
				return nil
			}); err != nil {
				test.Fatal(err)
			}
			if after := legacyDerivedOriginalImagePG(test, storage, ctx, fixture); after != original {
				test.Fatal("directory finalization or publication rewrote original source ownership or identity")
			}
		})
	}
}
