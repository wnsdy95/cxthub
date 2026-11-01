//go:build postgres

package store

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestDocSpoolQuantumPGPrepareDoesNotCopySourceBeforeFirstStep(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture := legacyByteStagePGSource(test, storage, ctx, 2<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	peer, trace := docSpoolWorkPGStore(test, storage, ctx)
	prepared, err := peer.PrepareDocRangeWork(ctx, job)
	if err != nil || prepared == nil {
		test.Fatal("public preparation did not return resumable capture", err)
	}
	defer func() {
		if err := prepared.Close(); err != nil {
			test.Error(err)
		}
	}()
	capture, ok := prepared.(*preparedLegacyCaptureWork)
	if !ok || capture.work != nil || capture.source == nil {
		test.Fatal("public preparation did not leave source capture pending", prepared)
	}
	before, err := prepared.RetainedResources()
	if err != nil || before.SpoolBytes != 0 || before.HeapBytes <= 0 || before.HeapBytes > 1<<20 || trace.sourceReads.Load() != 0 {
		test.Fatal("public Prepare consumed or retained the complete source", before, trace.sourceReads.Load(), err)
	}
	if err := prepared.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("empty local preparation accepted publication", err)
	}
	captureQuanta := docSpoolWorkPGSourceQuanta(len(fixture.raw))
	for step := int64(1); step <= captureQuanta; step++ {
		if progress, err := prepared.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
			test.Fatal("capture span failed or sealed prematurely", step, progress, err)
		}
		after, err := prepared.RetainedResources()
		if err != nil || after.SpoolBytes != min(step*domain.ConversationManifestChunkBytes, int64(len(fixture.raw))) ||
			after.HeapBytes > 1<<20 || capture.work != nil || trace.sourceReads.Load() != step || trace.privateWrites.Load() != 0 {
			test.Fatal("capture copied beyond one stored span, retained raw heap, or started local spooling", step, after, err)
		}
		if err := prepared.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			test.Fatal("unverified captured source accepted publication", step, err)
		}
	}
	path := capture.source.file.Name()
	docCaptureWorkPGDecode(test, capture, ctx, job)
	witness := capture.witness
	if progress, err := prepared.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending ||
		capture.work != nil || capture.source != nil || len(capture.parts) != 0 || capture.witness != witness ||
		capture.canonicalProof.Valid() || capture.original || trace.sourceReads.Load() != captureQuanta {
		test.Fatal("post-decode release did not yield with only the decoded source and its witness", progress, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("post-decode release retained the stored capture file", err)
	}
	inputQuanta := int(docSpoolWorkPGSourceQuanta(len(fixture.raw)))
	outputQuanta := int(docSpoolWorkPGSourceQuanta(len(fixture.stream)))
	proofQuanta := inputQuanta + 1 + 2*outputQuanta + 1
	for step := 1; step <= proofQuanta; step++ {
		progress, err := prepared.Step(ctx, job)
		if err != nil || progress != outbound.DocRangeWorkPending || capture.work != nil || capture.original ||
			capture.canonicalProof.Valid() != (step == proofQuanta) {
			test.Fatal("canonical proof did not yield through input, decision, copy, verify, and seal quanta", step, proofQuanta, progress, err)
		}
		expectedBytes := min(max(step-inputQuanta-1, 0), outputQuanta) * domain.ConversationManifestChunkBytes
		expectedBytes = min(expectedBytes, len(fixture.stream))
		if capture.proofData == nil && expectedBytes != 0 || capture.proofData != nil && capture.proofData.size != expectedBytes {
			test.Fatal("canonical producer did not append exactly one event-stream block per copy step", step, expectedBytes)
		}
		resources, err := prepared.RetainedResources()
		if err != nil || resources.HeapBytes >= 16<<20 || resources.SpoolBytes != int64(len(fixture.raw)+expectedBytes) ||
			trace.sourceReads.Load() != captureQuanta || trace.privateWrites.Load() != 0 {
			test.Fatal("canonical proof hid its spools, retained a whole raw proof, reread SQL, or copied private bytes", step, resources, err)
		}
		if err := prepared.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			test.Fatal("provisional or sealed source proof skipped its separate metadata handoff", step, err)
		}
		if legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
			test.Fatal("canonical proof quantum changed persisted private or public state", step)
		}
	}
	data := capture.proofData
	source, supported := capture.canonicalProof.Source().(*legacyProofSpool)
	if !supported || source == nil || source.legacyDocSpool != data {
		test.Fatal("canonical proof did not bind the exact output spool being handed off")
	}
	if progress, err := prepared.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending ||
		capture.work == nil || capture.work.data != data || capture.work.spooling != nil || capture.work.preparation == nil ||
		capture.work.preparation.ObjectCount() != 0 || capture.work.metadata.Valid() || capture.decoding != nil ||
		capture.canonical != nil || capture.canonicalProof.Valid() || capture.proofData != nil {
		test.Fatal("canonical handoff duplicated its verified spool or restarted eager local spooling", progress, err)
	}
	before, err = prepared.RetainedResources()
	if err != nil || before.SpoolBytes != int64(len(fixture.stream)) || before.HeapBytes >= 1<<20 {
		test.Fatal("canonical handoff retained raw proof state or emitted metadata", before, err)
	}
	if legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
		test.Fatal("canonical candidate construction changed persisted private or public state")
	}
	for ordinal := 0; ordinal < outputQuanta; ordinal++ {
		want, _ := legacyByteStagePGBlock(fixture, ordinal)
		body, err := data.Read(ctx, ordinal*domain.ConversationManifestChunkBytes, len(want))
		if err != nil || !bytes.Equal(body, want) {
			test.Fatal("canonical source-backed handoff changed exact event-stream bytes", ordinal, err)
		}
	}
	if progress, err := prepared.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
		test.Fatal("first post-handoff metadata quantum failed or sealed prematurely", progress, err)
	}
	after, err := prepared.RetainedResources()
	if err != nil || after.HeapBytes >= 16<<20 || after.SpoolBytes >= 64<<20 || capture.work.data != data ||
		capture.work.spooling != nil || trace.sourceReads.Load() != captureQuanta || trace.privateWrites.Load() == 0 {
		test.Fatal("first post-handoff quantum restarted local copying instead of metadata and private-byte work", after, err)
	}
	if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
		test.Fatal("private source-backed metadata quantum advertised public readiness", ready, err)
	}
}
