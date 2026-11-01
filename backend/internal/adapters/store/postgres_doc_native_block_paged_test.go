//go:build postgres

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docNativeBlockPGCount = 8192

type docNativeBlockPGState struct {
	decoder        uintptr
	sourceParts    uintptr
	native         uintptr
	object         uintptr
	list           uintptr
	child          uintptr
	position       int64
	objectPosition int64
	listPosition   int64
	childPosition  int64
	end            int64
	phase          uint64
	childPhase     uint64
	blockType      string
	blockText      string
	paged          docNativeEventsPGPagedState
}

func docNativeBlockPGText(ordinal int) string {
	return fmt.Sprintf("block-%05d:%s한🙂\"\\\n<>&", ordinal, strings.Repeat("x", 128))
}

func docNativeBlockPGSource(test *testing.T, storage *PostgresStore, ctx context.Context) (legacyByteStagePGFixture, []byte) {
	test.Helper()
	if domain.ConversationManifestChunkBytes != 512<<10 || domain.LegacyNormalizationPartBytes != 64<<10 || domain.LegacyCanonicalEncodingBytes != 64<<10 {
		test.Fatal("native block capture changed its original proof, normalization, or canonical page geometry")
	}
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	blocks := make([]domain.ContentBlock, docNativeBlockPGCount)
	for ordinal := range blocks {
		blocks[ordinal] = domain.ContentBlock{Type: "text", Text: docNativeBlockPGText(ordinal)}
	}
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
		SessionOriginID: string(repo), Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{
		{Kind: domain.EventMessage, Role: domain.RoleAssistant, Blocks: blocks},
	}}
	fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
	var stored bytes.Buffer
	if err := json.Indent(&stored, fixture.raw, "", "  "); err != nil {
		test.Fatal(err)
	}
	if len(fixture.raw) <= 2*domain.ConversationManifestChunkBytes || stored.Len() <= len(fixture.raw) || bytes.Equal(stored.Bytes(), fixture.raw) {
		test.Fatal("native block fixture lost its multi-proof-part noncanonical stored representation")
	}
	var decoded domain.CIRDocument
	if err := json.Unmarshal(stored.Bytes(), &decoded); err != nil || len(decoded.Events) != 1 || len(decoded.Events[0].Blocks) != docNativeBlockPGCount {
		test.Fatal("independent typed decoder rejected the stored native block fixture", err)
	}
	oracle := legacyMonolithicVerifierProof(test, decoded)
	if oracle.Hash() != fixture.hash || !bytes.Equal(oracle.Bytes(), fixture.raw) {
		test.Fatal("stored native blocks changed the independently verified canonical bytes or SHA")
	}
	return fixture, stored.Bytes()
}

func docNativeBlockPGRecordPayload(test *testing.T, record reflect.Value) (int64, int64) {
	test.Helper()
	block, heaps, excess := record.FieldByName("block"), record.FieldByName("heaps"), record.FieldByName("excess")
	if heaps.Kind() != reflect.Array || heaps.Len() != 2 || excess.Kind() != reflect.Array || excess.Len() != 2 {
		test.Fatal("native block page lost its fixed per-field payload and excess records")
	}
	var payloadBytes, excessBytes int64
	for index, name := range []string{"Type", "Text"} {
		heap := heaps.Index(index)
		var minimum rootPublicationHeapEstimate
		minimum.allocation(int64(block.FieldByName(name).Len()), 1)
		if minimum.err != nil || !heap.FieldByName("err").IsNil() || excess.Index(index).Int() < 0 ||
			heap.FieldByName("bytes").Int() < minimum.bytes || heap.FieldByName("bytes").Int()-minimum.bytes != excess.Index(index).Int() {
			test.Fatal("native block field ledger differs from its actual text and retained excess", name)
		}
		payloadBytes += heap.FieldByName("bytes").Int()
		excessBytes += excess.Index(index).Int()
	}
	return payloadBytes, excessBytes
}

func docNativeBlockPGPagedSnapshot(test *testing.T, work *preparedLegacyCaptureWork, object, list reflect.Value, field int) docNativeEventsPGPagedState {
	test.Helper()
	data := list.FieldByName("data")
	if !data.IsValid() || data.IsNil() {
		test.Fatal("active public PG block decoder lost its native paged data")
	}
	value := data.Elem()
	records := value.FieldByName("records")
	payload, estimate, borrowed := value.FieldByName("payload"), list.FieldByName("estimate"), list.FieldByName("borrowed")
	fieldHeap := object.FieldByName("heaps").Index(field)
	state := docNativeEventsPGPagedState{values: data.Pointer(), root: records.FieldByName("root").Pointer(),
		single: records.FieldByName("single").Pointer(), inline: records.FieldByName("inline").Pointer(),
		count: value.FieldByName("count").Int(), records: records.FieldByName("count").Int(), pages: records.FieldByName("pages").Int(),
		nodes: records.FieldByName("nodes").Int(), depth: records.FieldByName("depth").Int(), payload: payload.FieldByName("bytes").Int(),
		estimate: estimate.FieldByName("bytes").Int(), borrowed: borrowed.FieldByName("bytes").Int()}
	if state.count != list.FieldByName("count").Int() || state.count <= 0 || state.count >= docNativeBlockPGCount ||
		state.records < state.count || state.records > state.count+1 || state.records > docNativeBlockPGCount || state.depth < 0 || state.depth > 2 ||
		state.payload < 0 || state.borrowed < 0 || !payload.FieldByName("err").IsNil() || !estimate.FieldByName("err").IsNil() ||
		!borrowed.FieldByName("err").IsNil() || !fieldHeap.FieldByName("err").IsNil() || state.borrowed != fieldHeap.FieldByName("bytes").Int() {
		test.Fatal("active native block data lost its logical count, highwater, payload, or borrowed field heap", state)
	}
	var payloadBytes, visited int64
	backing := docCanonicalPagesPGRecordHeap(test, records, func(record reflect.Value, ordinal int64) {
		block := record.FieldByName("block")
		if ordinal < state.count &&
			(block.FieldByName("Type").String() != "text" || block.FieldByName("Text").String() != docNativeBlockPGText(int(ordinal))) {
			test.Fatal("active native block page changed a committed source slot", ordinal)
		}
		payload, _ := docNativeBlockPGRecordPayload(test, record)
		payloadBytes += payload
		visited++
	})
	backing.allocation(1, int64(value.Type().Size()))
	if state.root != 0 && (state.records <= 8 || state.pages != (state.records+63)/64) {
		test.Fatal("native block page count differs from its actual reserved highwater", state)
	}
	var storage rootPublicationHeapEstimate
	storage.allocation(1, int64(list.FieldByName("decoded").Type().Size()))
	state.backing = backing.bytes
	if backing.err != nil || storage.err != nil || visited != state.records ||
		payloadBytes != state.payload || state.estimate != storage.bytes+state.backing+state.payload {
		test.Fatal("active PG native blocks omitted actual fixed backing or field payloads from their heap ledger", state, backing, payloadBytes, visited)
	}
	if resources, err := work.RetainedResources(); err != nil || resources.HeapBytes < state.estimate {
		test.Fatal("actual PG capture omitted active native block pages from retained resources", resources, state, err)
	}
	return state
}

func docNativeBlockPGSnapshot(test *testing.T, work *preparedLegacyCaptureWork, stored []byte) docNativeBlockPGState {
	test.Helper()
	docCanonicalPagesPGScratch(test, work)
	preparation := reflect.ValueOf(work.normalization.preparation).Elem()
	decoder := preparation.FieldByName("decoder")
	if decoder.IsNil() || preparation.FieldByName("pending").Len() != 0 || preparation.FieldByName("written").Int() != 0 ||
		preparation.FieldByName("count").Int() != 0 {
		test.Fatal("native block witness escaped raw decoding into canonical preparation or output")
	}
	native := decoder.Elem().FieldByName("native")
	if native.IsNil() {
		return docNativeBlockPGState{}
	}
	object := native.Elem().FieldByName("object")
	if object.IsNil() || native.Elem().FieldByName("canonicalReady").Bool() {
		test.Fatal("native block witness is not an unfinished raw event")
	}
	if !object.Elem().FieldByName("blockActive").Bool() {
		return docNativeBlockPGState{}
	}
	field := int(object.Elem().FieldByName("field").Int())
	lists := object.Elem().FieldByName("blocks")
	if field < 0 || field >= lists.Len() || lists.Index(field).IsNil() {
		test.Fatal("active public PG block decoder lost its selected block list")
	}
	metadata := object.Elem().FieldByName("metadata").Elem().FieldByName("fields").Index(field)
	list := lists.Index(field)
	value := list.Elem()
	paged := value.FieldByName("paged")
	if metadata.FieldByName("name").String() != "blocks" || !metadata.FieldByName("blockList").Bool() ||
		!object.Elem().FieldByName("pagedEvents").Bool() || !paged.IsValid() || !paged.Bool() ||
		!native.Elem().FieldByName("decoded").FieldByName("Blocks").IsNil() || value.FieldByName("copied").Int() != 0 {
		test.Fatal("actual public PG native blocks were not decoded in paged mode")
	}
	for _, name := range []string{"decoded", "previous", "payloads", "previousHeaps", "excess", "previousExtra"} {
		array := value.FieldByName(name)
		if !array.IsValid() || array.Kind() != reflect.Slice || !array.IsNil() || array.Cap() != 0 {
			test.Fatal("actual public PG block decoding retained a typed value or ledger array", name)
		}
	}
	if value.FieldByName("count").Int() == 0 {
		return docNativeBlockPGState{}
	}
	source := decoder.Elem().FieldByName("source")
	docNativeEventsPGSourceGeometry(test, source, stored, domain.LegacyNormalizationPartBytes)
	state := docNativeBlockPGState{decoder: decoder.Pointer(), sourceParts: source.FieldByName("Parts").Pointer(),
		native: native.Pointer(), object: object.Pointer(), list: list.Pointer(), position: decoder.Elem().FieldByName("position").Int(),
		objectPosition: object.Elem().FieldByName("position").Int(), listPosition: value.FieldByName("position").Int(),
		end: value.FieldByName("end").Int(), phase: value.FieldByName("phase").Uint(),
		blockType: value.FieldByName("block").FieldByName("Type").String(), blockText: value.FieldByName("block").FieldByName("Text").String(),
		paged: docNativeBlockPGPagedSnapshot(test, work, object.Elem(), value, field)}
	if state.listPosition >= state.end {
		test.Fatal("active native block witness reached the end of its raw source")
	}
	if child := value.FieldByName("object"); !child.IsNil() {
		state.child, state.childPosition, state.childPhase = child.Pointer(), child.Elem().FieldByName("position").Int(), child.Elem().FieldByName("phase").Uint()
	}
	return state
}

func docNativeBlockPGPublish(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture, stored []byte,
	work *preparedLegacyCaptureWork, job domain.DocRangeIndexJob, trace *docCaptureWorkPGTrace, bodyReads int64, original string) {
	test.Helper()
	progress := outbound.DocRangeWorkPending
	var err error
	canonicalWitnessed := false
	for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && work.work == nil; quantum++ {
		progress, err = work.Step(ctx, job)
		if err != nil || progress != outbound.DocRangeWorkPending {
			break
		}
		if !canonicalWitnessed && work.normalization != nil && work.normalization.canonical != nil {
			source := reflect.ValueOf(work.normalization.canonical).Elem().FieldByName("source")
			docNativeEventsPGSourceGeometry(test, source, fixture.raw, domain.ConversationManifestChunkBytes)
			canonicalWitnessed = true
		}
	}
	if err != nil || progress != outbound.DocRangeWorkPending || work.work == nil || !canonicalWitnessed {
		test.Fatal("native block capture did not reach its verified same-process handoff", progress, err, canonicalWitnessed)
	}
	for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && progress != outbound.DocRangeWorkPublishable; quantum++ {
		progress, err = work.Step(ctx, job)
		if err != nil || progress == outbound.DocRangeWorkCompatibilityRequired {
			test.Fatal("native block capture failed delegated publication preparation", quantum, progress, err)
		}
	}
	if progress != outbound.DocRangeWorkPublishable || generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
		test.Fatal("native block capture stalled or exposed a generation before publication", progress)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		_, err := storage.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
		if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
			return nil
		}
		return errors.Join(domain.ErrIntegrity, err)
	}); err != nil {
		test.Fatal("private native block preparation became readable", err)
	}
	if trace.bodyReads.Load() != bodyReads || original != legacyDerivedOriginalImagePG(test, storage, ctx, fixture) {
		test.Fatal("native block preparation reread SQL bodies or changed immutable archival tuples")
	}
	if err := work.Publish(ctx, job); err != nil {
		test.Fatal("publish paged native blocks", err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
	if completed := docRangeJobPGGet(test, storage, ctx, fixture.repo, fixture.hash); completed.Version != job.Version {
		test.Fatal("native block publication completed a different job version", completed.Version, job.Version)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		reader, err := storage.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
		if err != nil {
			return err
		}
		generation := reader.Descriptor()
		if generation.Hash != fixture.hash || generation.EventCount != 1 || generation.Identity() != domain.DocumentIdentityLegacy ||
			generation.ByteStorage() != domain.DocRangeByteStorageDerived || generation.StreamBytes != int64(len(fixture.stream)) ||
			!bytes.Equal(generation.Envelope, fixture.envelope) || generation.ChunkCount() != (len(fixture.stream)+domain.ConversationManifestChunkBytes-1)/domain.ConversationManifestChunkBytes {
			test.Fatal("native block publication changed its canonical binding or original output chunk geometry")
		}
		var stream []byte
		for ordinal := 0; ordinal < generation.ChunkCount(); ordinal++ {
			want, descriptor := legacyByteStagePGBlock(fixture, ordinal)
			body, err := storage.ReadLegacyDerivedByteChunk(read, fixture.repo, fixture.hash, reader.Binding(), ordinal, descriptor)
			if err != nil {
				return err
			}
			if !bytes.Equal(body, want) || domain.HashContent(body) != descriptor.Hash {
				test.Fatal("published native block chunk changed its exact canonical bytes or SHA", ordinal)
			}
			stream = append(stream, body...)
		}
		raw := domain.AssembleCanonicalDoc(generation.Envelope, [][]byte{stream})
		if !bytes.Equal(stream, fixture.stream) || !bytes.Equal(raw, fixture.raw) || domain.HashContent(raw) != fixture.hash || sha256.Sum256(raw) != sha256.Sum256(fixture.raw) {
			test.Fatal("published paged native blocks changed the independently verified canonical bytes or SHA")
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	var source []byte
	if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, fixture.hash).Scan(&source); err != nil || !bytes.Equal(source, stored) {
		test.Fatal("native block publication changed the noncanonical archival bytes", err)
	}
	if trace.bodyReads.Load() != bodyReads || original != legacyDerivedOriginalImagePG(test, storage, ctx, fixture) {
		test.Fatal("native block publication reread SQL source bodies or rewrote immutable archival tuples")
	}
}

func TestDocNativeBlockPGActivePagesRetainFencesAndPublish(test *testing.T) {
	for _, scenario := range []struct {
		name          string
		change        string
		canceled      bool
		argumentFence bool
	}{
		{name: "unchanged"},
		{name: "source version", change: "source version"},
		{name: "source incarnation", change: "source incarnation"},
		{name: "ownership", change: "ownership"},
		{name: "lease expiry", change: "lease expiry"},
		{name: "job fence", change: "job fence"},
		{name: "snapshot identity", change: "snapshot identity"},
		{name: "argument fence", argumentFence: true},
		{name: "cancellation", canceled: true},
		{name: "cancellation before source conflict", change: "source version", canceled: true},
		{name: "cancellation before ownership failure", change: "ownership", canceled: true},
		{name: "cancellation before argument fence", canceled: true, argumentFence: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture, stored := docNativeBlockPGSource(test, storage, ctx)
			work, job, trace := docNormalizationPGStartSource(test, storage, ctx, fixture, stored)
			source, err := work.decoding.proofSource(ctx)
			if err != nil {
				test.Fatal("public native block capture lost its original proof source", err)
			}
			docNativeEventsPGSourceGeometry(test, reflect.ValueOf(source), stored, domain.ConversationManifestChunkBytes)
			bodyReads := trace.bodyReads.Load()
			private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			original := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
			inspection := &docNativeEventsPGContext{Context: ctx}
			var before docNativeBlockPGState
			witnessed := 0
			for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && witnessed < 2; quantum++ {
				loads := inspection.loads
				if progress, err := work.Step(inspection, job); err != nil || progress != outbound.DocRangeWorkPending {
					test.Fatal("actual public native block decoding did not yield pending", quantum, progress, err)
				}
				if inspection.loads-loads < 0 || inspection.loads-loads > 2 {
					test.Fatal("native block decoding exceeded its original two authenticated source loads", quantum, inspection.loads-loads)
				}
				after := docNativeBlockPGSnapshot(test, work, stored)
				if after.list == 0 || after.paged.pages < 2 {
					if before.list != 0 {
						test.Fatal("native block decoding finished before consecutive active quanta were observed")
					}
					continue
				}
				if before.list != 0 {
					if after.decoder != before.decoder || after.sourceParts != before.sourceParts || after.native != before.native ||
						after.object != before.object || after.list != before.list || after.position < before.position ||
						after.listPosition < before.listPosition || after == before || after.paged.values != before.paged.values ||
						after.paged.count < before.paged.count || after.paged.records < before.paged.records || after.paged.pages < before.paged.pages ||
						after.paged.nodes < before.paged.nodes || after.paged.payload < before.paged.payload || after.paged.estimate < before.paged.estimate ||
						after.paged.borrowed != before.paged.borrowed {
						test.Fatal("active PG block quantum restarted or lost source progress, fixed pages, or retained payloads", before, after)
					}
					witnessed++
				}
				before = after
			}
			if witnessed != 2 || inspection.loads == 0 || before.paged.pages < 2 || !reflect.DeepEqual(work.job, job) {
				test.Fatal("public PG native block decoding did not retain multiple fixed pages across consecutive pending quanta", witnessed, inspection.loads, before)
			}
			if trace.bodyReads.Load() != bodyReads || private != legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) ||
				public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) || generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
				test.Fatal("unfinished native block decoding reread SQL payload or changed private/public persisted state")
			}
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("unfinished native block decoding became publishable", err)
			}
			if scenario.name == "unchanged" {
				docNativeBlockPGPublish(test, storage, ctx, fixture, stored, work, job, trace, bodyReads, original)
				return
			}
			candidate := job
			if scenario.argumentFence {
				candidate.Version++
			}
			if scenario.change != "" {
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, scenario.change)
				if scenario.change == "source incarnation" {
					if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, stored); err != nil {
						test.Fatal(err)
					}
					current, _, err := storage.readLegacyCaptureSource(ctx, job, nil, 0, 0)
					if err != nil || current.stage.sourceID == work.witness.stage.sourceID || current.stage.sourceVersion != work.witness.stage.sourceVersion || current.bytes != len(stored) {
						test.Fatal("reincarnated native block source did not preserve its byte extent and version", current, err)
					}
				}
			}
			selected, cancel := context.WithCancel(ctx)
			defer cancel()
			want := error(domain.ErrConflict)
			if scenario.change == "ownership" || scenario.change == "snapshot identity" {
				want = domain.ErrNotFound
			}
			if scenario.canceled {
				cancel()
				want = context.Canceled
			}
			private = legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			public = legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			scratch := docCanonicalPagesPGScratch(test, work)
			resources, err := work.RetainedResources()
			if err != nil {
				test.Fatal(err)
			}
			normalization, preparation := work.normalization, work.normalization.preparation
			queries := trace.queries.Load()
			fenced := &docNativeEventsPGContext{Context: selected}
			progress, err := work.Step(fenced, candidate)
			if !errors.Is(err, want) || progress != 0 || work.normalization != normalization || work.normalization.preparation != preparation ||
				docNativeBlockPGSnapshot(test, work, stored) != before || docCanonicalPagesPGScratch(test, work) != scratch || !reflect.DeepEqual(work.job, job) {
				test.Fatal("revoked active PG block quantum advanced its decoder, data pages, scratch, or job", scenario.name, progress, err, want)
			}
			if fenced.loads != 0 {
				test.Fatal("revoked active PG block quantum resumed authenticated normalization source loads", scenario.name, fenced.loads)
			}
			if after, err := work.RetainedResources(); err != nil || after != resources {
				test.Fatal("revoked active PG block quantum changed retained resources", resources, after, err)
			}
			if trace.bodyReads.Load() != bodyReads || (scenario.canceled || scenario.argumentFence) && trace.queries.Load() != queries {
				test.Fatal("revoked active PG block quantum reread SQL payload or pre-SQL rejection queried the database")
			}
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("revoked active PG block quantum became publishable", err)
			}
			if private != legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) || public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) ||
				generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
				test.Fatal("revoked active PG block quantum changed persisted source, jobs, staging, or public generation")
			}
		})
	}
}
