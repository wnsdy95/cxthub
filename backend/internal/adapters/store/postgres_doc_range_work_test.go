//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"unsafe"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func docRangeWorkPGPrepare(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob) *preparedLegacyByteWork {
	test.Helper()
	private := legacyByteStagePGImage(test, storage, ctx, job.DocHash, -1)
	prepared, err := storage.PrepareDocRangeWork(ctx, job)
	if err != nil || prepared == nil {
		test.Fatal("prepare real capture or reverified durable recovery", err)
	}
	test.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			test.Error(err)
		}
	})
	var work *preparedLegacyByteWork
	if recovery, supported := prepared.(*preparedLegacyRecoveryWork); supported {
		for step := 0; recovery.work == nil && step < docCaptureWorkPGPreparationStepLimit; step++ {
			if progress, err := recovery.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
				test.Fatal("durable recovery did not freshly verify before metadata handoff", progress, err)
			}
		}
		if recovery.work == nil || recovery.input != nil || recovery.data != nil || recovery.canonical != nil || recovery.parts != nil {
			test.Fatal("durable recovery retained input state or failed its bounded handoff")
		}
		work, recovery.work = recovery.work, nil
		if err := recovery.Close(); err != nil {
			test.Fatal(err)
		}
	} else {
		capture, supported := prepared.(*preparedLegacyCaptureWork)
		if !supported {
			test.Fatalf("public preparation returned unsupported work: %T", prepared)
		}
		docCaptureWorkPGBeforeHandoff(test, capture, ctx, job)
		source, supported := capture.canonicalProof.Source().(*legacyProofSpool)
		if !capture.canonicalProof.Valid() || capture.original || capture.proofData == nil || !supported || source == nil || source.legacyDocSpool != capture.proofData {
			test.Fatal("canonical public preparation did not produce a source-backed proof")
		}
		data := capture.proofData
		if progress, err := capture.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || capture.work == nil {
			test.Fatal("canonical public proof did not hand off in its separate step", progress, err)
		}
		work = capture.work
		if capture.source != nil || len(capture.parts) != 0 || capture.decoding != nil || capture.canonical != nil ||
			capture.canonicalProof.Valid() || capture.proofData != nil || work.spooling != nil || work.data != data {
			test.Fatal("canonical public handoff retained capture state, restarted local spooling, or replaced the verified data spool")
		}
	}
	if after := legacyByteStagePGImage(test, storage, ctx, job.DocHash, -1); after != private {
		defer work.Close()
		test.Fatal("public canonical preparation changed private byte progress")
	}
	objects, err := work.objects.file.Stat()
	if err != nil {
		defer work.Close()
		test.Fatal(err)
	}
	if work.preparation == nil || work.metadata.Valid() || work.session.proof.Valid() || objects.Size() != 0 ||
		len(work.objects.offsets) != 1 || work.objects.offsets[0] != 0 || work.objects.writer.Buffered() != 0 {
		defer work.Close()
		test.Fatal("preparation sealed metadata, emitted objects, or retained its canonical proof before the first step")
	}
	if metadata, err := work.preparation.Metadata(); !errors.Is(err, domain.ErrDocRangeIndexPending) || metadata.Valid() {
		defer work.Close()
		test.Fatal("unstepped preparation exposed sealed metadata", metadata.Valid(), err)
	}
	return work
}

func docRangeWorkPGClose(test *testing.T, work *preparedLegacyByteWork) {
	test.Helper()
	if err := work.Close(); err != nil {
		test.Error("close prepared legacy byte work", err)
	}
}

func docRangeWorkPGResources(test *testing.T, work *preparedLegacyByteWork, streamBytes int) outbound.RootPublicationResources {
	test.Helper()
	resources, err := work.RetainedResources()
	if err != nil {
		test.Fatal(err)
	}
	data, err := work.data.file.Stat()
	if err != nil {
		test.Fatal(err)
	}
	objects, err := work.objects.file.Stat()
	if err != nil {
		test.Fatal(err)
	}
	planBytes, err := work.session.plan.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	var metadataBytes int64
	objectCount := work.metadata.ObjectCount()
	if work.spooling != nil {
		if work.preparation != nil || work.metadata.Valid() || reflect.ValueOf(work.header).IsZero() {
			test.Fatal("local spooling retained metadata preparation or lost its envelope seed")
		}
		headerBytes, err := work.header.RetainedBytes()
		if err != nil {
			test.Fatal(err)
		}
		spoolingBytes, err := work.spooling.RetainedBytes()
		if err != nil {
			test.Fatal(err)
		}
		metadataBytes = headerBytes + spoolingBytes
	} else if work.preparation != nil {
		if work.metadata.Valid() {
			test.Fatal("pending preparation also retained sealed metadata")
		}
		metadataBytes, err = work.preparation.RetainedBytes()
		objectCount = work.preparation.ObjectCount()
	} else {
		metadataBytes, err = work.metadata.RetainedBytes()
	}
	if err != nil {
		test.Fatal(err)
	}
	minimumHeap := planBytes + metadataBytes + int64(unsafe.Sizeof(*work)) + int64(unsafe.Sizeof(*work.data)) +
		int64(unsafe.Sizeof(*work.objects)) + int64(cap(work.session.encoded)) +
		int64(cap(work.objects.offsets))*int64(unsafe.Sizeof(int64(0))) + int64(work.objects.writer.Size())
	if work.session.proof.Valid() || !work.session.plan.Valid() {
		test.Fatal("prepared work retained a duplicate canonical proof or lost its plan")
	}
	if work.spooling != nil {
		if planBytes < int64(streamBytes) {
			test.Fatal("raw plan omitted its retained canonical source", planBytes, streamBytes)
		}
	} else {
		if streamBytes > 0 && planBytes >= int64(streamBytes) {
			test.Fatal("verified local spool retained its raw source plan", planBytes, streamBytes)
		}
		if !reflect.ValueOf(work.header).IsZero() {
			if work.recoveryReady || work.preparation == nil || work.header.Reference() != work.preparation.Reference() || work.header.PlanKey() != work.preparation.PlanKey() {
				test.Fatal("header checkpoint retained a seed without its matching preparation")
			}
			header := reflect.ValueOf(work.header).FieldByName("envelope")
			envelope := reflect.ValueOf(work.preparation).Elem().FieldByName("generation").FieldByName("Envelope")
			if header.Len() != envelope.Len() || header.Cap() != envelope.Cap() || header.Pointer() != envelope.Pointer() {
				test.Fatal("header checkpoint retained a second envelope backing outside the preparation ledger")
			}
		}
	}
	if len(work.objects.offsets) == 0 || len(work.objects.offsets)-1 != objectCount ||
		work.objects.offsets[len(work.objects.offsets)-1] != objects.Size() || work.objects.writer.Buffered() != 0 ||
		(work.spooling == nil && work.preparation == nil && streamBytes > 0 && objects.Size() <= 0) ||
		(work.spooling != nil && objects.Size() != 0) {
		test.Fatal("metadata objects were not flushed or do not match the sealed capsule", objects.Size(), work.objects.offsets)
	}
	if data.Size() != int64(work.data.size) || data.Size() < 0 || data.Size() > int64(streamBytes) ||
		(work.spooling == nil && data.Size() != int64(streamBytes)) || objects.Size() < 0 ||
		resources.SpoolBytes != data.Size()+objects.Size() || resources.SpoolBytes >= 64<<20 ||
		resources.HeapBytes < minimumHeap || (work.spooling == nil && resources.HeapBytes >= 16<<20) {
		test.Fatalf("resources=%+v minimum heap=%d actual data=%d metadata=%d", resources, minimumHeap, data.Size(), objects.Size())
	}
	if !data.Mode().IsRegular() || !objects.Mode().IsRegular() || data.Mode().Perm() != 0600 || objects.Mode().Perm() != 0600 {
		test.Fatal("prepared work did not retain private regular spool files", data.Mode(), objects.Mode())
	}
	return resources
}

func docRangeWorkPGFinish(test *testing.T, work *preparedLegacyByteWork, ctx context.Context, job domain.DocRangeIndexJob, streamBytes, steps int) {
	test.Helper()
	docRangeWorkPGResources(test, work, streamBytes)
	sourceQuanta := max((streamBytes+domain.ConversationManifestChunkBytes-1)/domain.ConversationManifestChunkBytes, 1)
	for step := steps + 1; step <= sourceQuanta; step++ {
		complete, err := work.Step(ctx, job)
		if err != nil || complete != outbound.DocRangeWorkPending {
			test.Fatal("source quantum completed metadata before directory finalization", step, sourceQuanta, complete, err)
		}
		docRangeWorkPGAssertMetadataPending(test, work, ctx, job)
		docRangeWorkPGResources(test, work, streamBytes)
	}
	if work.preparation == nil || work.metadata.Valid() {
		test.Fatal("final source quantum did not leave directory finalization pending")
	}
	eventPages := work.preparation.ObjectCount()
	chunkPages := (work.session.stage.blocks + domain.DocRangeChunkPageSize - 1) / domain.DocRangeChunkPageSize
	directorySteps := docRangeWorkPGDirectoryObjects(eventPages) + chunkPages + docRangeWorkPGDirectoryObjects(chunkPages)
	if eventPages == 0 || chunkPages == 0 || directorySteps < 3 {
		test.Fatal("nonempty fixture did not retain event pages and pending directory objects", eventPages, chunkPages, directorySteps)
	}
	for step := 1; step <= directorySteps; step++ {
		docRangeWorkPGAssertMetadataPending(test, work, ctx, job)
		before := work.preparation.ObjectCount()
		complete, err := work.Step(ctx, job)
		wantProgress := outbound.DocRangeWorkPending
		if step == directorySteps {
			wantProgress = outbound.DocRangeWorkPublishable
		}
		if err != nil || complete != wantProgress {
			test.Fatal("directory finalization did not stop after one ordered object", step, directorySteps, complete, err)
		}
		after := work.metadata.ObjectCount()
		if work.preparation != nil {
			after = work.preparation.ObjectCount()
		}
		if emitted := after - before; emitted < 0 || emitted > 1 {
			test.Fatal("directory step emitted more than one metadata object", step, before, after)
		}
		if work.metadata.Valid() != (complete == outbound.DocRangeWorkPublishable) || (work.preparation == nil) != (complete == outbound.DocRangeWorkPublishable) {
			test.Fatal("metadata did not remain pending until its final chunk directory", step, complete)
		}
		if complete == outbound.DocRangeWorkPending {
			docRangeWorkPGAssertMetadataPending(test, work, ctx, job)
		}
		docRangeWorkPGResources(test, work, streamBytes)
	}
	if objects := work.metadata.ObjectCount(); objects != eventPages+directorySteps {
		test.Fatal("directory finalization omitted an ordered metadata object", objects, eventPages, directorySteps)
	}
}

func docRangeWorkPGDirectoryObjects(leaves int) int {
	objects := 0
	for leaves > 0 {
		leaves = (leaves + domain.DocRangeDirectoryFanout - 1) / domain.DocRangeDirectoryFanout
		objects += leaves
		if leaves == 1 {
			break
		}
	}
	return objects
}

func docRangeWorkPGAssertMetadataPending(test *testing.T, work *preparedLegacyByteWork, ctx context.Context, job domain.DocRangeIndexJob) {
	test.Helper()
	if work.preparation == nil || work.metadata.Valid() {
		test.Fatal("pending work exposed sealed metadata")
	}
	if metadata, err := work.preparation.Metadata(); !errors.Is(err, domain.ErrDocRangeIndexPending) || metadata.Valid() {
		test.Fatal("incomplete directory finalization exposed metadata", metadata.Valid(), err)
	}
	if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("incomplete directory finalization accepted publication", err)
	}
}

func docRangeWorkPGAssertClosed(test *testing.T, work *preparedLegacyByteWork, ctx context.Context, job domain.DocRangeIndexJob) {
	test.Helper()
	for attempt := 0; attempt < 2; attempt++ {
		if err := work.Close(); err != nil {
			test.Fatal("close was not successful and idempotent", attempt, err)
		}
	}
	for _, file := range []*os.File{work.data.file, work.objects.file} {
		if _, err := os.Stat(file.Name()); !errors.Is(err, os.ErrNotExist) {
			test.Fatal("close did not remove a spool", file.Name(), err)
		}
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			test.Fatal("close left a spool descriptor open", file.Name(), err)
		}
	}
	if complete, err := work.Step(ctx, job); complete == outbound.DocRangeWorkPublishable || !errors.Is(err, os.ErrClosed) {
		test.Fatal("closed work accepted another step", complete, err)
	}
	if err := work.Publish(ctx, job); !errors.Is(err, os.ErrClosed) {
		test.Fatal("closed work accepted publication", err)
	}
	if resources, err := work.RetainedResources(); !errors.Is(err, os.ErrClosed) || resources != (outbound.RootPublicationResources{}) {
		test.Fatal("closed work reported retained resources", resources, err)
	}
	if work.session.plan.Valid() || work.session.proof.Valid() || work.metadata.Valid() || work.preparation != nil ||
		work.spooling != nil || !reflect.ValueOf(work.header).IsZero() || work.session.encoded != nil {
		test.Fatal("closed work retained its plan, proof, or metadata")
	}
}

func docRangeWorkPGCorruptByte(test *testing.T, file *os.File, offset int64) func() {
	test.Helper()
	original := make([]byte, 1)
	if _, err := file.ReadAt(original, offset); err != nil {
		test.Fatal(err)
	}
	write := func(body []byte) {
		test.Helper()
		if written, err := file.WriteAt(body, offset); err != nil || written != len(body) {
			test.Fatal("write local spool corruption or repair", written, err)
		}
	}
	write([]byte{original[0] ^ 1})
	return func() { write(original) }
}

func TestDocRangeWorkPGPrepareBoundedStepsPublishAndClose(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	directory := rangeSpoolTempDir(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	original := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	work := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, work)
	if work.session.stage.blocks != 11 || work.session.stage.next != 0 || work.session.stage.complete {
		test.Fatal("fixture did not prepare exactly eleven uncopied byte blocks", work.session.stage)
	}
	resources := docRangeWorkPGResources(test, work, len(fixture.stream))
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 2 {
		test.Fatal("preparation did not create exactly two local spools", len(files), err)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("preparation persisted a private header, object, or part")
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("preparation changed source, ownership, snapshot, job, or public ranges")
	}
	if counts := generationCountsPG(test, storage, ctx, fixture.hash); counts != [2]int{} {
		test.Fatal("preparation published range metadata", counts)
	}
	if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
		test.Fatal("first step did not stop at the eight-block boundary", complete, err)
	}
	if work.preparation == nil || work.metadata.Valid() || len(work.objects.offsets) != 1 {
		test.Fatal("first giant-record quantum prematurely sealed metadata or emitted an object")
	}
	firstResources := docRangeWorkPGResources(test, work, len(fixture.stream))
	if firstResources.HeapBytes < domain.ConversationManifestChunkBytes || firstResources.SpoolBytes != resources.SpoolBytes {
		test.Fatal("first metadata quantum did not account for its actual retained buffer and empty object spool", resources, firstResources)
	}
	stage := work.session.stage
	stage.next = 8
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	prefix := legacyByteStagePGImage(test, storage, ctx, fixture.hash, stage.next)
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("first private step changed public or archival tuples")
	}
	if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
		test.Fatal("second step did not leave metadata pending after copying the remaining three blocks", complete, err)
	}
	stage.next, stage.complete = 11, true
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, 8); after != prefix {
		test.Fatal("second step rewrote an acknowledged prefix part or object")
	}
	private = legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("complete byte copying did not leave publication pending on metadata", err)
	}
	docRangeWorkPGFinish(test, work, ctx, job, len(fixture.stream), 2)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("metadata-only quanta rewrote complete private byte progress")
	}
	if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPublishable {
		test.Fatal("completed work did not acknowledge an idempotent terminal step", complete, err)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("terminal step rewrote a private tuple")
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("complete private work advertised or mutated public state before publication")
	}
	if after := docRangeWorkPGResources(test, work, len(fixture.stream)); after.SpoolBytes <= resources.SpoolBytes {
		test.Fatal("completed metadata did not account for its newly flushed object spool", resources, after)
	}
	if err := work.Publish(ctx, job); err != nil {
		test.Fatal(err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		reader, err := storage.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
		if err != nil {
			return err
		}
		generation := reader.Descriptor()
		if generation.Version != 5 || generation.Identity() != domain.DocumentIdentityLegacy ||
			generation.ByteStorage() != domain.DocRangeByteStorageDerived || generation.ChunkCount() != 11 {
			test.Fatal("publication changed the derived generation identity", generation)
		}
		var stream []byte
		for ordinal := 0; ordinal < generation.ChunkCount(); ordinal++ {
			want, descriptor := legacyByteStagePGBlock(fixture, ordinal)
			body, err := storage.ReadLegacyDerivedByteChunk(read, fixture.repo, fixture.hash, reader.Binding(), ordinal, descriptor)
			if err != nil {
				return err
			}
			if !bytes.Equal(body, want) {
				test.Fatal("published byte block differs from its canonical source range", ordinal)
			}
			stream = append(stream, body...)
		}
		if !bytes.Equal(domain.AssembleCanonicalDoc(fixture.envelope, [][]byte{stream}), fixture.raw) {
			test.Fatal("published byte reads do not reconstruct the original canonical document")
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if after := legacyDerivedOriginalImagePG(test, storage, ctx, fixture); after != original {
		test.Fatal("preparation, steps, or publication rewrote archival tuples")
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("publication rewrote the complete private stage")
	}
	docRangeWorkPGAssertClosed(test, work, ctx, job)
	if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
		test.Fatal("close left local spool files", len(files), err)
	}
}

func TestDocRangeWorkPGDataSpoolCorruptionRollsBackStepAndRetries(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	rangeSpoolTempDir(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	work := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, work)
	for _, boundary := range []struct {
		corruptOrdinal int
		next           int
		complete       bool
	}{
		{corruptOrdinal: 7, next: 8},
		{corruptOrdinal: 10, next: 11, complete: true},
	} {
		private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
		public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
		resources := docRangeWorkPGResources(test, work, len(fixture.stream))
		repair := docRangeWorkPGCorruptByte(test, work.data.file, int64(boundary.corruptOrdinal)*domain.ConversationManifestChunkBytes)
		if complete, err := work.Step(ctx, job); complete == outbound.DocRangeWorkPublishable || !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("corrupt local byte block advanced the bounded step", boundary.corruptOrdinal, complete, err)
		}
		if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
			test.Fatal("failed byte validation committed a header, copied object, part, or checkpoint", boundary.corruptOrdinal)
		}
		if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
			test.Fatal("failed byte validation changed public or archival state", boundary.corruptOrdinal)
		}
		if after := docRangeWorkPGResources(test, work, len(fixture.stream)); after != resources {
			test.Fatal("failed byte validation advanced local metadata preparation", resources, after)
		}
		repair()
		if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
			test.Fatal("repaired spool could not retry the same bounded step", boundary.corruptOrdinal, complete, err)
		}
		stage := work.session.stage
		stage.next, stage.complete = boundary.next, boundary.complete
		legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	}
	docRangeWorkPGFinish(test, work, ctx, job, len(fixture.stream), 2)
	if err := work.Publish(ctx, job); err != nil {
		test.Fatal("repaired data spool could not publish", err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
}

func TestDocRangeWorkPGMetadataQuantumVerifiesNextSourceBlock(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	rangeSpoolTempDir(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	work := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, work)
	stage := work.session.stage
	for step := 1; step <= 2; step++ {
		if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
			test.Fatal("byte checkpoint incorrectly completed pending metadata", step, complete, err)
		}
		stage.next = min(step*8, stage.blocks)
		stage.complete = stage.next == stage.blocks
		stage.incarnation = work.session.stage.incarnation
		legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
		docRangeWorkPGResources(test, work, len(fixture.stream))
	}
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	docRangeWorkPGCorruptByte(test, work.data.file, 2*domain.ConversationManifestChunkBytes)
	if complete, err := work.Step(ctx, job); complete == outbound.DocRangeWorkPublishable || !errors.Is(err, domain.ErrPreparedDocRangeObject) {
		test.Fatal("third metadata quantum did not verify the third source block after byte copying completed", complete, err)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("failed metadata source quantum changed complete durable byte progress")
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("failed metadata source quantum changed public or archival state")
	}
	docRangeWorkPGAssertClosed(test, work, ctx, job)
	replacement := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, replacement)
	docRangeWorkPGFinish(test, replacement, ctx, job, len(fixture.stream), 0)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("replacement metadata scan rewrote the already complete durable byte prefix")
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("replacement metadata scan advertised public readiness before publication")
	}
	if err := replacement.Publish(ctx, job); err != nil {
		test.Fatal("replacement could not publish after restarting metadata", err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
}

func TestDocRangeWorkPGMetadataSpoolCorruptionRollsBackPublishAndRetries(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	rangeSpoolTempDir(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
	var verifier domain.CanonicalDocVerifier
	proof, err := verifier.Verify(ctx, fixture.hash, fixture.raw)
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, fixture.repo, func(bound context.Context) error {
		return storage.putLegacyRangeGeneration(bound, fixture.repo, proof)
	}); err != nil {
		test.Fatal(err)
	}
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	work := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, work)
	docRangeWorkPGFinish(test, work, ctx, job, len(fixture.stream), 0)
	stage := work.session.stage
	stage.next, stage.complete = stage.blocks, true
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	lastObject := work.metadata.ObjectCount() - 1
	if lastObject < 0 {
		test.Fatal("fixture has no range metadata to corrupt")
	}
	repair := docRangeWorkPGCorruptByte(test, work.objects.file, work.objects.offsets[lastObject])
	if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("corrupt local metadata was published", err)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("failed metadata validation committed generation replacement, object deletion, or job completion")
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("failed metadata publication lost or rewrote complete private progress")
	}
	repair()
	if err := work.Publish(ctx, job); err != nil {
		test.Fatal("repaired metadata could not publish the existing complete private stage", err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("publication retry rewrote complete private progress")
	}
}

func TestDocRangeWorkPGPublishRequiresBothFlags(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	rangeSpoolTempDir(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	work := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, work)
	docRangeWorkPGFinish(test, work, ctx, job, len(fixture.stream), 0)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	for _, flags := range []struct {
		name               string
		bytePublication    bool
		derivedPublication bool
	}{
		{name: "byte-disabled", derivedPublication: true},
		{name: "derived-disabled", bytePublication: true},
		{name: "both-disabled"},
	} {
		test.Run(flags.name, func(test *testing.T) {
			storage.ConfigureLegacyByteRangePublication(flags.bytePublication)
			storage.ConfigureLegacyDerivedByteRangePublication(flags.derivedPublication)
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("prepared work bypassed final publication feature flags", err)
			}
			if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
				test.Fatal("disabled publication changed public or archival state")
			}
			if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
				test.Fatal("disabled publication changed complete private progress")
			}
		})
	}
	storage.ConfigureLegacyByteRangePublication(true)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	if err := work.Publish(ctx, job); err != nil {
		test.Fatal("re-enabled publication could not reuse prepared complete work", err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
}

func TestDocRangeWorkPGReprepareReverifiesSourceAndResumesPrefix(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	rangeSpoolTempDir(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	trace := &docSpoolWorkPGTrace{}
	config := storage.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	peer := &PostgresStore{pool: pool}
	peer.ConfigureLegacyByteRangePublication(true)
	peer.ConfigureLegacyDerivedByteRangePublication(true)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	first := docRangeWorkPGPrepare(test, peer, ctx, job)
	defer docRangeWorkPGClose(test, first)
	if reads := trace.sourceReads.Load(); reads != docSpoolWorkPGSourceQuanta(len(fixture.raw)) {
		test.Fatal("initial preparation did not read each original stored span exactly once", reads)
	}
	if complete, err := first.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
		test.Fatal("first prepared work did not park after eight blocks", complete, err)
	}
	firstResources := docRangeWorkPGResources(test, first, len(fixture.stream))
	if first.preparation == nil || first.metadata.Valid() {
		test.Fatal("first prepared handle did not leave metadata pending")
	}
	firstPreparationBytes, err := first.preparation.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	stage := first.session.stage
	stage.next = 8
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	prefix := legacyByteStagePGImage(test, storage, ctx, fixture.hash, stage.next)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	if reads := trace.sourceReads.Load(); reads != docSpoolWorkPGSourceQuanta(len(fixture.raw)) {
		test.Fatal("bounded step reread the original source", reads)
	}
	docRangeWorkPGAssertClosed(test, first, ctx, job)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("closing local work removed or rewrote its persisted prefix")
	}
	resumed := docRangeWorkPGPrepare(test, peer, ctx, job)
	defer docRangeWorkPGClose(test, resumed)
	if reads := trace.sourceReads.Load(); reads != 2*docSpoolWorkPGSourceQuanta(len(fixture.raw)) {
		test.Fatal("new preparation reused a discarded source proof instead of reverifying", reads)
	}
	resources := docRangeWorkPGResources(test, resumed, len(fixture.stream))
	if resources.SpoolBytes != int64(len(fixture.stream)) {
		test.Fatal("replacement preparation reused metadata from the discarded handle", resources)
	}
	if resumed.session.stage.plan != stage.plan || resumed.session.stage.sourceID != stage.sourceID ||
		resumed.session.stage.sourceVersion != stage.sourceVersion || resumed.session.stage.blocks != stage.blocks {
		test.Fatal("new preparation changed the unmodified source incarnation or plan", resumed.session.stage, stage)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("new preparation rewrote existing private progress")
	}
	if complete, err := resumed.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
		test.Fatal("replacement step must finish three remaining byte blocks but restart metadata", complete, err)
	}
	if after := docRangeWorkPGResources(test, resumed, len(fixture.stream)); after.SpoolBytes != firstResources.SpoolBytes ||
		after.HeapBytes < domain.ConversationManifestChunkBytes || resumed.preparation == nil || resumed.metadata.Valid() {
		test.Fatal("replacement did not restart its first metadata quantum", firstResources, after)
	}
	if retained, err := resumed.preparation.RetainedBytes(); err != nil || retained != firstPreparationBytes {
		test.Fatal("replacement metadata retained a different first-quantum buffer", firstPreparationBytes, retained, err)
	}
	stage.next, stage.complete = 11, true
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, 8); after != prefix {
		test.Fatal("resumed work rewrote an acknowledged prefix tuple")
	}
	private = legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	docRangeWorkPGFinish(test, resumed, ctx, job, len(fixture.stream), 1)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("restarted metadata preparation rewrote durable byte progress")
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("close, preparation, or resumed private work changed public or archival state")
	}
	if err := resumed.Publish(ctx, job); err != nil {
		test.Fatal(err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
	if reads := trace.sourceReads.Load(); reads != 2*docSpoolWorkPGSourceQuanta(len(fixture.raw)) {
		test.Fatal("resumed step or publication reread the original source", reads)
	}
}

func TestDocRangeWorkPGChangedSourceRejectsFrozenSpoolAndRebuilds(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	rangeSpoolTempDir(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	stale := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, stale)
	if complete, err := stale.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
		test.Fatal("fixture did not reach its first committed checkpoint", complete, err)
	}
	docRangeWorkPGFinish(test, stale, ctx, job, len(fixture.stream), 1)
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash); err != nil {
		test.Fatal(err)
	}
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	if complete, err := stale.Step(ctx, job); complete == outbound.DocRangeWorkPublishable || !errors.Is(err, domain.ErrConflict) {
		test.Fatal("frozen source was copied after the physical source version changed", complete, err)
	}
	if err := stale.Publish(ctx, job); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("frozen source bypassed the final physical source fence", err)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("stale frozen work changed the durable checkpoint")
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("stale frozen work published or changed current archival state")
	}
	fresh := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, fresh)
	if fresh.session.stage.sourceVersion <= stale.session.stage.sourceVersion {
		test.Fatal("replacement preparation retained the obsolete physical source", fresh.session.stage, stale.session.stage)
	}
	if complete, err := fresh.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
		test.Fatal("new physical source did not rebuild its own first eight blocks", complete, err)
	}
	docRangeWorkPGFinish(test, fresh, ctx, job, len(fixture.stream), 1)
	if err := fresh.Publish(ctx, job); err != nil {
		test.Fatal(err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
}

func TestDocRangeWorkPGCompatibilityReadsRootOnlyForSelectionAndCurrentVerification(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 0)
	plan, chunked := domain.PlanDocChunks(fixture.raw)
	if !chunked || plan.Manifest.Format != domain.ChunkFormatV2 {
		test.Fatal("fixture is not a current legacy chunk manifest")
	}
	if _, _, err := storage.PutChunks(ctx, fixture.repo, plan.Bodies); err != nil {
		test.Fatal(err)
	}
	manifest, err := json.Marshal(plan.Manifest)
	if err != nil {
		test.Fatal(err)
	}
	stored := docCompress(manifest)
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, stored); err != nil {
		test.Fatal(err)
	}
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	trace := &docSpoolWorkPGTrace{}
	config := storage.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	peer := &PostgresStore{pool: pool}
	peer.ConfigureLegacyByteRangePublication(true)
	peer.ConfigureLegacyDerivedByteRangePublication(true)
	prepared, err := peer.PrepareDocRangeWork(ctx, job)
	if err != nil || prepared == nil || trace.sourceReads.Load() != 0 {
		test.Fatal("chunked source was read or declined before provisional capture", prepared, trace.sourceReads.Load(), err)
	}
	test.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			test.Error(err)
		}
	})
	capture, ok := prepared.(*preparedLegacyCaptureWork)
	if !ok || capture.work != nil || capture.source == nil {
		test.Fatalf("chunked source skipped provisional capture: %T", prepared)
	}
	docCaptureWorkPGCopy(test, capture, ctx, job)
	docCaptureWorkPGDecode(test, capture, ctx, job)
	docCaptureWorkPGBeforeHandoff(test, capture, ctx, job)
	if !capture.original || capture.canonicalProof.Valid() {
		test.Fatal("manifest did not preserve the original full-classifier handoff")
	}
	progress, err := prepared.Step(ctx, job)
	if err != nil {
		test.Fatal("chunked source capture or decode failed", err)
	}
	storedReads := docSpoolWorkPGSourceQuanta(len(stored))
	if progress != outbound.DocRangeWorkCompatibilityRequired || trace.sourceReads.Load() != storedReads {
		test.Fatal("chunked source did not request compatibility after one bounded stored capture", progress, trace.sourceReads.Load(), storedReads)
	}
	if err := prepared.Close(); err != nil {
		test.Fatal("close private capture before compatibility", err)
	}
	if err := peer.BuildDocRangeCompatibilityJob(ctx, job); err != nil {
		test.Fatal("current legacy compatibility verification failed", err)
	}
	if trace.sourceReads.Load() != storedReads+1 {
		test.Fatal("compatibility reentered preparation or skipped current locked root verification", trace.sourceReads.Load())
	}
	if done := docRangeJobPGGet(test, peer, ctx, fixture.repo, fixture.hash); done.State != "completed" {
		test.Fatal("compatibility path did not complete the actual range job", done)
	}
	var version int
	if err := storage.pool.QueryRow(ctx, `SELECT range_layout_version FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&version); err != nil || version != 4 {
		test.Fatal("compatibility path changed original-chunk provenance", version, err)
	}
}

func TestDocRangeWorkPGNearDecodeCapRetainsOnlySpools(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	directory := rangeSpoolTempDir(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture := legacyByteStagePGSource(test, storage, ctx, maxLegacyReadBytes-(16<<10))
	if len(fixture.raw) > maxLegacyReadBytes || len(fixture.raw) < maxLegacyReadBytes-(16<<10) {
		test.Fatal("fixture did not approach the unchanged decoded-source cap", len(fixture.raw), maxLegacyReadBytes)
	}
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	work := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, work)
	resources := docRangeWorkPGResources(test, work, len(fixture.stream))
	if work.session.stage.blocks != 64 || resources.HeapBytes >= 1<<20 || resources.SpoolBytes <= 31<<20 {
		test.Fatal("near-cap source was not separated from bounded retained heap", work.session.stage.blocks, resources)
	}
	if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
		test.Fatal("near-cap source did not stop at its first bounded copy batch", complete, err)
	}
	if after := docRangeWorkPGResources(test, work, len(fixture.stream)); after.HeapBytes < domain.ConversationManifestChunkBytes ||
		after.SpoolBytes != resources.SpoolBytes || work.metadata.Valid() || work.preparation == nil || len(work.objects.offsets) != 1 {
		test.Fatal("near-cap first quantum hid its buffer or eagerly prepared giant-record metadata", resources, after)
	}
	stage := work.session.stage
	stage.next = 8
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	docRangeWorkPGAssertClosed(test, work, ctx, job)
	if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
		test.Fatal("near-cap work leaked local spool files", len(files), err)
	}
	test.Logf("decoded_source=%d retained_heap=%d spool_bytes=%d blocks=%d", len(fixture.raw), resources.HeapBytes, resources.SpoolBytes, stage.blocks)
}
