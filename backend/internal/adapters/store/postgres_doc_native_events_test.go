//go:build postgres

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docNativeEventsPGState struct {
	decoder          uintptr
	sourceParts      uintptr
	native           uintptr
	object           uintptr
	list             uintptr
	cursor           uintptr
	decoded          uintptr
	previous         uintptr
	position         int64
	objectPosition   int64
	listPosition     int64
	childPosition    int64
	count            int64
	copied           int64
	end              int64
	decodedCapacity  int
	previousCapacity int
	listPhase        uint64
	childPhase       uint64
	paged            docNativeEventsPGPagedState
}

func docNativeEventsPGSourceGeometry(test *testing.T, source reflect.Value, raw []byte, partBytes int) {
	test.Helper()
	actual := source.FieldByName("PartBytes").Int()
	if actual == 0 && partBytes == domain.ConversationManifestChunkBytes {
		actual = int64(domain.ConversationManifestChunkBytes)
	}
	parts := source.FieldByName("Parts")
	if source.FieldByName("Bytes").Int() != int64(len(raw)) || actual != int64(partBytes) || source.FieldByName("Read").IsNil() ||
		parts.Len() != (len(raw)+partBytes-1)/partBytes {
		test.Fatal("actual legacy source changed its byte extent, reader, or authentication geometry", source.FieldByName("Bytes").Int(), actual, parts.Len(), len(raw), partBytes)
	}
	for ordinal := 0; ordinal < parts.Len(); ordinal++ {
		part := parts.Index(ordinal)
		offset := ordinal * partBytes
		length := min(partBytes, len(raw)-offset)
		if part.FieldByName("Bytes").Int() != int64(length) || part.FieldByName("Hash").String() != string(domain.HashContent(raw[offset:offset+length])) {
			test.Fatal("actual legacy source descriptor does not authenticate its original byte interval", ordinal, offset, length)
		}
	}
}

func docNativeEventsPGSnapshot(test *testing.T, work *preparedLegacyCaptureWork, stored []byte) docNativeEventsPGState {
	test.Helper()
	docCanonicalPagesPGScratch(test, work)
	preparation := reflect.ValueOf(work.normalization.preparation).Elem()
	decoder := preparation.FieldByName("decoder")
	if decoder.IsNil() || preparation.FieldByName("pending").Len() != 0 || preparation.FieldByName("written").Int() != 0 ||
		preparation.FieldByName("count").Int() != 0 {
		test.Fatal("native event-list witness passed raw decoding into canonical preparation or output")
	}
	native := decoder.Elem().FieldByName("native")
	if native.IsNil() {
		return docNativeEventsPGState{}
	}
	object := native.Elem().FieldByName("object")
	if object.IsNil() || native.Elem().FieldByName("canonicalReady").Bool() {
		test.Fatal("native event-list witness is not an unfinished raw event")
	}
	active := object.Elem().FieldByName("eventActive")
	if !active.IsValid() {
		test.Fatal("actual raw decoder does not contain native event-list support")
	}
	if !active.Bool() {
		return docNativeEventsPGState{}
	}
	source := decoder.Elem().FieldByName("source")
	docNativeEventsPGSourceGeometry(test, source, stored, domain.LegacyNormalizationPartBytes)
	field := int(object.Elem().FieldByName("field").Int())
	lists := object.Elem().FieldByName("events")
	if field < 0 || field >= lists.Len() || lists.Index(field).IsNil() {
		test.Fatal("active raw event-list decoder lost its selected list")
	}
	metadata := object.Elem().FieldByName("metadata").Elem().FieldByName("fields").Index(field)
	if metadata.FieldByName("name").String() != "replacement" || !metadata.FieldByName("eventList").Bool() ||
		native.Elem().FieldByName("decoded").FieldByName("Replacement").Len() != 0 {
		test.Fatal("raw replacement was committed before its actual child decoder finished")
	}
	list := lists.Index(field)
	value := list.Elem()
	cursor := value.FieldByName("cursor")
	if cursor.IsNil() {
		cursor = list
	}
	child := cursor.Elem().FieldByName("object")
	if cursor.Pointer() != list.Pointer() || child.IsNil() || value.FieldByName("position").Int() >= value.FieldByName("end").Int() {
		test.Fatal("flat raw replacement lost its unfinished same-process child cursor")
	}
	decoded, previous := value.FieldByName("decoded"), value.FieldByName("previous")
	if !object.Elem().FieldByName("pagedEvents").Bool() || !child.Elem().FieldByName("pagedEvents").Bool() ||
		!value.FieldByName("paged").Bool() || !decoded.IsNil() || !previous.IsNil() || value.FieldByName("copied").Int() != 0 ||
		!native.Elem().FieldByName("decoded").FieldByName("Replacement").IsNil() || value.FieldByName("records").FieldByName("count").Int() != 0 {
		test.Fatal("actual raw replacement retained a typed array instead of its paged native values")
	}
	return docNativeEventsPGState{
		decoder: decoder.Pointer(), native: native.Pointer(), object: object.Pointer(), list: list.Pointer(), cursor: cursor.Pointer(),
		sourceParts: source.FieldByName("Parts").Pointer(),
		decoded:     decoded.Pointer(), previous: previous.Pointer(), position: decoder.Elem().FieldByName("position").Int(),
		objectPosition: object.Elem().FieldByName("position").Int(), listPosition: value.FieldByName("position").Int(),
		childPosition: child.Elem().FieldByName("position").Int(), count: value.FieldByName("count").Int(),
		copied: value.FieldByName("copied").Int(), end: value.FieldByName("end").Int(),
		decodedCapacity: decoded.Cap(), previousCapacity: previous.Cap(),
		listPhase: value.FieldByName("phase").Uint(), childPhase: child.Elem().FieldByName("phase").Uint(),
		paged: docNativeEventsPGPagedSnapshot(test, work, object.Elem(), value, field),
	}
}

func docNativeEventsPGSource(test *testing.T, storage *PostgresStore, ctx context.Context) (legacyByteStagePGFixture, []byte) {
	test.Helper()
	if domain.ConversationManifestChunkBytes != 512<<10 || domain.LegacyNormalizationPartBytes != 64<<10 || domain.LegacyCanonicalEncodingBytes != 64<<10 {
		test.Fatal("raw replacement changed its original proof, native source, or canonical page quantum")
	}
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	text := strings.Repeat("x", 64)
	children := make([]domain.CIREvent, 8192)
	for ordinal := range children {
		children[ordinal] = domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleAssistant,
			Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
	}
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
		SessionOriginID: string(repo), Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{
		{Kind: domain.EventCompaction, Replacement: children, ReplacementComplete: true},
	}}
	fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
	child := `{"kind":"message","role":"assistant","blocks":[{"type":"text","text":"` + text + `"}]}`
	event := `{"kind":"compaction","replacement":[` + strings.Repeat(child+",", len(children)-1) + child + `],"replacement_complete":true}`
	stored := domain.AssembleCanonicalDoc(fixture.envelope, [][]byte{[]byte(event)})
	var decoded domain.CIRDocument
	if len(event) != 1138753 || len(stored) <= 2*domain.ConversationManifestChunkBytes || bytes.Equal(stored, fixture.raw) {
		test.Fatal("raw replacement fixture lost its actual 8192-child, 64-byte-text source extent", len(event), len(stored))
	}
	if err := json.Unmarshal(stored, &decoded); err != nil || len(decoded.Events) != 1 || len(decoded.Events[0].Replacement) != len(children) {
		test.Fatal("original typed decoder rejected the raw replacement fixture", err)
	}
	oracle := legacyMonolithicVerifierProof(test, decoded)
	if oracle.Hash() != fixture.hash || !bytes.Equal(oracle.Bytes(), fixture.raw) {
		test.Fatal("raw replacement fixture changed the independent typed canonical bytes or SHA")
	}
	return fixture, stored
}

func TestDocNativeEventsPGRawDecodeRetainsFencesAndPublishes(test *testing.T) {
	for _, change := range []string{"unchanged", "lease expiry", "source version", "ownership", "cancellation", "job fence", "source incarnation", "snapshot identity", "argument fence"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture, stored := docNativeEventsPGSource(test, storage, ctx)
			work, job, trace := docNormalizationPGStartSource(test, storage, ctx, fixture, stored)
			source, err := work.decoding.proofSource(ctx)
			if err != nil {
				test.Fatal("raw event-list capture lost its original canonical proof source", err)
			}
			docNativeEventsPGSourceGeometry(test, reflect.ValueOf(source), stored, domain.ConversationManifestChunkBytes)
			bodyReads := trace.bodyReads.Load()
			private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			original := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
			var before docNativeEventsPGState
			inspection := &docNativeEventsPGContext{Context: ctx}
			witnessed := 0
			for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && witnessed < 2; quantum++ {
				loads := inspection.loads
				if progress, err := work.Step(inspection, job); err != nil || progress != outbound.DocRangeWorkPending {
					test.Fatal("actual raw replacement decoding did not yield pending", quantum, progress, err)
				}
				if inspection.loads-loads < 0 || inspection.loads-loads > 2 {
					test.Fatal("actual raw replacement exceeded its original two authenticated source loads", quantum, inspection.loads-loads)
				}
				after := docNativeEventsPGSnapshot(test, work, stored)
				if after.list == 0 || after.count == 0 || after.count >= 8192 {
					if before.list != 0 {
						test.Fatal("actual raw replacement decoding finished before consecutive active quanta were observed")
					}
					continue
				}
				if before.list != 0 {
					if after.decoder != before.decoder || after.sourceParts != before.sourceParts || after.native != before.native ||
						after.object != before.object || after.list != before.list || after.cursor != before.cursor ||
						after.position < before.position || after.count < before.count || after.listPosition < before.listPosition || after == before {
						test.Fatal("raw replacement quantum restarted or failed to advance its actual source decoder", before, after)
					}
					if after.paged.values != before.paged.values || after.paged.count < before.paged.count || after.paged.records < before.paged.records ||
						after.paged.pages < before.paged.pages || after.paged.nodes < before.paged.nodes || after.paged.payload < before.paged.payload ||
						after.paged.estimate < before.paged.estimate || after.paged.borrowed != before.paged.borrowed {
						test.Fatal("raw replacement quantum restarted or lost its actual paged values or heap ledger", before.paged, after.paged)
					}
					witnessed++
				}
				before = after
			}
			if witnessed != 2 || !reflect.DeepEqual(work.job, job) {
				test.Fatal("same-job raw replacement decoding was not witnessed across consecutive pending quanta", witnessed)
			}
			if inspection.loads == 0 || before.paged.pages < 2 {
				test.Fatal("actual raw replacement did not authenticate source parts and retain multiple fixed value pages", inspection.loads, before.paged)
			}
			if trace.bodyReads.Load() != bodyReads || private != legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) ||
				public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) || generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
				test.Fatal("unfinished raw event-list decoding reread SQL payload or changed private/public persisted state")
			}
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("unfinished raw event-list decoding became publishable", err)
			}
			if change == "unchanged" {
				progress := outbound.DocRangeWorkPending
				var err error
				canonicalWitnessed := false
				for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit+8192 && work.work == nil; quantum++ {
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
					test.Fatal("raw event-list decoding did not reach its same-process verified handoff", progress, err)
				}
				for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && progress != outbound.DocRangeWorkPublishable; quantum++ {
					progress, err = work.Step(ctx, job)
					if err != nil || progress == outbound.DocRangeWorkCompatibilityRequired {
						test.Fatal("raw event-list control failed delegated preparation", quantum, progress, err)
					}
				}
				if progress != outbound.DocRangeWorkPublishable || generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
					test.Fatal("raw event-list control stalled or exposed a public generation before publication", progress)
				}
				if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
					_, err := storage.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
					if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
						return nil
					}
					return errors.Join(domain.ErrIntegrity, err)
				}); err != nil {
					test.Fatal("private raw event-list control became readable", err)
				}
				if err := work.Publish(ctx, job); err != nil {
					test.Fatal("publish raw event-list control", err)
				}
				legacyDerivedPublishedPG(test, storage, ctx, fixture)
				if completed := docRangeJobPGGet(test, storage, ctx, fixture.repo, fixture.hash); completed.Version != job.Version {
					test.Fatal("raw event-list control completed a different job version", completed.Version, job.Version)
				}
				if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
					reader, err := storage.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
					if err != nil {
						return err
					}
					generation := reader.Descriptor()
					if generation.Hash != fixture.hash || generation.EventCount != 1 || generation.Identity() != domain.DocumentIdentityLegacy ||
						generation.ByteStorage() != domain.DocRangeByteStorageDerived || generation.StreamBytes != int64(len(fixture.stream)) {
						test.Fatal("raw event-list publication changed its canonical document binding")
					}
					stream := docPayloadProjectionPGReadRange(test, storage, read, fixture, reader, 0, len(fixture.stream))
					raw := domain.AssembleCanonicalDoc(generation.Envelope, [][]byte{stream})
					if !bytes.Equal(raw, fixture.raw) || domain.HashContent(raw) != fixture.hash || sha256.Sum256(raw) != sha256.Sum256(fixture.raw) {
						test.Fatal("published raw event-list chunks did not reconstruct the original canonical bytes and SHA")
					}
					return nil
				}); err != nil {
					test.Fatal(err)
				}
				var source []byte
				if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, fixture.hash).Scan(&source); err != nil || !bytes.Equal(source, stored) {
					test.Fatal("raw event-list publication changed the actual archival source bytes", err)
				}
				if trace.bodyReads.Load() != bodyReads || original != legacyDerivedOriginalImagePG(test, storage, ctx, fixture) {
					test.Fatal("raw event-list publication reread SQL source bodies or rewrote immutable archival tuples")
				}
				return
			}
			candidate := job
			if change == "argument fence" {
				candidate.Version++
			} else if change != "cancellation" {
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
				if change == "source incarnation" {
					if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, stored); err != nil {
						test.Fatal(err)
					}
					current, _, err := storage.readLegacyCaptureSource(ctx, job, nil, 0, 0)
					if err != nil || current.stage.sourceID == work.witness.stage.sourceID || current.stage.sourceVersion != work.witness.stage.sourceVersion || current.bytes != len(stored) {
						test.Fatal("reincarnated raw source did not preserve its byte extent and version", current, err)
					}
				}
			}
			selected, cancel := context.WithCancel(ctx)
			defer cancel()
			want := error(domain.ErrConflict)
			if change == "ownership" || change == "snapshot identity" {
				want = domain.ErrNotFound
			} else if change == "cancellation" {
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
				docNativeEventsPGSnapshot(test, work, stored) != before || docCanonicalPagesPGScratch(test, work) != scratch || !reflect.DeepEqual(work.job, job) {
				test.Fatal("revoked raw event-list quantum advanced its decoder, cursor, scratch, or job", change, progress, err, want)
			}
			if fenced.loads != 0 {
				test.Fatal("revoked raw event-list quantum resumed authenticated source loads", change, fenced.loads)
			}
			if after, err := work.RetainedResources(); err != nil || after != resources {
				test.Fatal("revoked raw event-list quantum changed retained resources", resources, after, err)
			}
			if trace.bodyReads.Load() != bodyReads || (change == "cancellation" || change == "argument fence") && trace.queries.Load() != queries {
				test.Fatal("revoked raw event-list quantum reread SQL payload or pre-SQL rejection queried the database")
			}
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("revoked raw event-list quantum became publishable", err)
			}
			if private != legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) || public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) ||
				generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
				test.Fatal("revoked raw event-list quantum changed persisted source, jobs, staging, or public generation")
			}
		})
	}
}
