//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docSourceHeaderPGState struct {
	preparation, envelope, native, encoding, sourceParts uintptr
	phase, nativePhase, listPhase, encodingPhase         uint64
	position, scalarPosition, listPosition, listCount    int64
	listCopied, encodingField, encodingModel             int64
	hashState                                            string
}

func docSourceHeaderPGSource(test *testing.T, storage *PostgresStore, ctx context.Context, headerBytes int) legacyByteStagePGFixture {
	test.Helper()
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	models := make([]string, 8192)
	for ordinal := range models {
		models[ordinal] = "m"
	}
	models[0], models[len(models)-1] = "<\ud55c🙂> \"\\\n", "last-model"
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
			SourceModel: strings.Repeat("m", headerBytes), SourceModels: models,
			SessionOriginID: string(repo), Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{
			{Kind: domain.EventMessage, Seq: 2, Role: domain.RoleUser,
				Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo) + strings.Repeat("x", 1<<20) + "\ud55c🙂\"\\\n<>&"}}},
			{Kind: domain.EventMessage, Seq: 9, Role: domain.RoleAssistant,
				Blocks: []domain.ContentBlock{{Type: "text", Text: "original answer"}}},
		},
	}
	fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
	if len(fixture.envelope) <= 2*domain.LegacyCanonicalEncodingBytes || len(fixture.envelope) <= headerBytes ||
		len(fixture.raw) <= 2*domain.ConversationManifestChunkBytes {
		test.Fatal("canonical header fixture lost its large scalar/list or original multi-part source", len(fixture.envelope), len(fixture.raw))
	}
	return fixture
}

func docSourceHeaderPGStart(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) (*preparedLegacyCaptureWork, domain.DocRangeIndexJob, *docCaptureWorkPGTrace) {
	test.Helper()
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	peer, trace := docCaptureWorkPGStore(test, storage, ctx)
	work := docCaptureWorkPGPrepare(test, peer, ctx, job)
	docCaptureWorkPGCopy(test, work, ctx, job)
	docCaptureWorkPGDecode(test, work, ctx, job)
	source, err := work.decoding.proofSource(ctx)
	if err != nil {
		test.Fatal("large header lost its original proof source", err)
	}
	docNativeEventsPGSourceGeometry(test, reflect.ValueOf(source), fixture.raw, domain.ConversationManifestChunkBytes)
	return work, job, trace
}

func docSourceHeaderPGSnapshot(test *testing.T, work *preparedLegacyCaptureWork, fixture legacyByteStagePGFixture) (docSourceHeaderPGState, string) {
	test.Helper()
	if work.normalization != nil || work.original || work.work != nil || work.canonicalProof.Valid() {
		test.Fatal("canonical header witness entered normalization, original fallback, or delegated work")
	}
	if work.canonical == nil {
		return docSourceHeaderPGState{}, ""
	}
	preparation := reflect.ValueOf(work.canonical).Elem()
	envelope := preparation.FieldByName("envelope")
	if !envelope.IsValid() {
		test.Fatal("actual canonical source candidate does not contain the incremental strict envelope helper")
	}
	if envelope.IsNil() {
		return docSourceHeaderPGState{}, ""
	}
	frame := preparation.FieldByName("frame")
	if preparation.FieldByName("phase").Uint() != 1 || preparation.FieldByName("position").Int() != int64(len(fixture.raw)) ||
		preparation.FieldByName("actual").String() != string(fixture.hash) ||
		frame.FieldByName("envelopeHash").String() != string(domain.HashContent(fixture.envelope)) ||
		preparation.FieldByName("header").Len() != 0 || work.proofData == nil || work.proofData.size != 0 {
		test.Fatal("incremental header started before original hashing or retained seed/output bytes before strict verification")
	}
	value := envelope.Elem()
	if value.FieldByName("expected").String() != frame.FieldByName("envelopeHash").String() ||
		value.FieldByName("end").Int()-value.FieldByName("from").Int() != int64(len(fixture.envelope)) {
		test.Fatal("strict header helper lost its exact raw-envelope hash or byte interval")
	}
	state := docSourceHeaderPGState{
		preparation: reflect.ValueOf(work.canonical).Pointer(), envelope: envelope.Pointer(),
		sourceParts: preparation.FieldByName("source").FieldByName("Parts").Pointer(),
		phase:       value.FieldByName("phase").Uint(), hashState: string(value.FieldByName("hashState").Bytes()),
	}
	stage := ""
	native := value.FieldByName("native")
	if !native.IsNil() {
		state.native = native.Pointer()
		state.position, state.nativePhase = native.Elem().FieldByName("position").Int(), native.Elem().FieldByName("phase").Uint()
		if scalar := native.Elem().FieldByName("scalar"); !scalar.IsNil() {
			state.scalarPosition = scalar.Elem().FieldByName("position").Int()
		}
		for _, list := range native.Elem().FieldByName("lists").Seq2() {
			if !list.IsNil() {
				state.listPosition, state.listCount = list.Elem().FieldByName("position").Int(), list.Elem().FieldByName("count").Int()
				state.listCopied, state.listPhase = list.Elem().FieldByName("copied").Int(), list.Elem().FieldByName("phase").Uint()
			}
		}
		if state.position > value.FieldByName("from").Int() && state.position < value.FieldByName("end").Int() {
			stage = "native"
		}
	}
	encoding := value.FieldByName("encoding")
	if !encoding.IsNil() {
		state.encoding = encoding.Pointer()
		state.encodingField, state.encodingModel = encoding.Elem().FieldByName("field").Int(), encoding.Elem().FieldByName("model").Int()
		state.encodingPhase = encoding.Elem().FieldByName("phase").Uint()
		if !native.IsNil() || value.FieldByName("decoded").FieldByName("SourceModel").Len() != 0 ||
			value.FieldByName("decoded").FieldByName("SourceModels").Len() != 0 {
			test.Fatal("strict header encoder retained duplicate native decoded values")
		}
		if state.hashState != "" {
			stage = "encoding"
		}
	}
	return state, stage
}

func docSourceHeaderPGPause(test *testing.T, work *preparedLegacyCaptureWork, ctx context.Context, job domain.DocRangeIndexJob, fixture legacyByteStagePGFixture, phase string) docSourceHeaderPGState {
	test.Helper()
	for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit; quantum++ {
		state, stage := docSourceHeaderPGSnapshot(test, work, fixture)
		if stage == phase {
			source := reflect.ValueOf(work.canonical).Elem().FieldByName("source")
			docNativeEventsPGSourceGeometry(test, source, fixture.raw, domain.ConversationManifestChunkBytes)
			return state
		}
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
			test.Fatal("canonical header did not yield inside its requested strict phase", phase, quantum, progress, err)
		}
	}
	test.Fatal("canonical header never exposed its actual unfinished strict phase", phase)
	return docSourceHeaderPGState{}
}

func TestDocSourceHeaderPGCandidatePublishesExactOriginal(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := docSourceHeaderPGSource(test, storage, ctx, 2*domain.LegacyCanonicalEncodingBytes+73)
	if len(fixture.envelope) >= domain.MaxConversationManifestBytes {
		test.Fatal("publishable header fixture exceeded the unchanged generation envelope limit", len(fixture.envelope))
	}
	oracle, err := verifyLegacyDecodedMonolithicDoc(ctx, fixture.hash, fixture.raw)
	if err != nil || !oracle.Valid() || !bytes.Equal(oracle.Bytes(), fixture.raw) {
		test.Fatal("independent original verifier rejected the canonical large header", err)
	}
	originalPlan, err := domain.NewLegacyByteBackfill(ctx, oracle)
	if err != nil {
		test.Fatal(err)
	}
	originalSeed, err := domain.NewLegacyRangeMetadataSeed(ctx, oracle, originalPlan)
	if err != nil {
		test.Fatal(err)
	}
	work, job, trace := docSourceHeaderPGStart(test, storage, ctx, fixture)
	bodyReads := trace.bodyReads.Load()
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	immutable := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
	native := docSourceHeaderPGPause(test, work, ctx, job, fixture, "native")
	encoding := docSourceHeaderPGPause(test, work, ctx, job, fixture, "encoding")
	if native.preparation != encoding.preparation || native.envelope != encoding.envelope || native.sourceParts != encoding.sourceParts {
		test.Fatal("large header restarted its actual canonical source or envelope at encoder handoff", native, encoding)
	}
	if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("unfinished strict canonical header became publishable", err)
	}
	if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
		test.Fatal("same-process header encoding did not resume", progress, err)
	}
	after, phase := docSourceHeaderPGSnapshot(test, work, fixture)
	if phase != "encoding" || after.encoding != encoding.encoding || after.envelope != encoding.envelope || after == encoding {
		test.Fatal("large strict header encoding restarted or did not advance across pending quanta", encoding, after, phase)
	}
	if resources, err := work.RetainedResources(); err != nil || resources.SpoolBytes != int64(len(fixture.raw)) || resources.HeapBytes >= 16<<20 {
		test.Fatal("strict header lost its accounted original spool or exceeded worker heap bounds", resources, err)
	}
	if trace.bodyReads.Load() != bodyReads || private != legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) ||
		public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) || generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
		test.Fatal("unfinished strict header reread SQL payload or changed source/job/private/public tuples")
	}
	sawSeedCopy := false
	for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && !work.canonicalProof.Valid(); quantum++ {
		preparation := reflect.ValueOf(work.canonical).Elem()
		if preparation.FieldByName("phase").Uint() == 5 {
			sawSeedCopy = true
			if !preparation.FieldByName("envelope").IsNil() || work.proofData.size != 0 {
				test.Fatal("final exact seed copy overlapped strict header verification or event-stream output")
			}
		}
		if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			test.Fatal("strict header or seed copy bypassed pending publication", err)
		}
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.normalization != nil || work.original || work.work != nil {
			test.Fatal("canonical header did not preserve its source-backed proof path", quantum, progress, err)
		}
	}
	if !sawSeedCopy || !work.canonicalProof.Valid() {
		test.Fatal("strict large header did not finish its separate original-byte seed copy and source proof")
	}
	plan, err := work.canonicalProof.Plan()
	if err != nil {
		test.Fatal(err)
	}
	seed, err := work.canonicalProof.Seed()
	if err != nil || !reflect.DeepEqual(seed, originalSeed) || !bytes.Equal(reflect.ValueOf(seed).FieldByName("envelope").Bytes(), fixture.envelope) {
		test.Fatal("incremental strict verification changed the exact original seed contract", err)
	}
	spec, specErr := plan.Spec()
	originalSpec, originalSpecErr := originalPlan.Spec()
	key, keyErr := plan.Key()
	originalKey, originalKeyErr := originalPlan.Key()
	if specErr != nil || originalSpecErr != nil || keyErr != nil || originalKeyErr != nil || spec != originalSpec || key != originalKey {
		test.Fatal("incremental strict header changed the original 512KiB backfill plan", spec, originalSpec, specErr, originalSpecErr, keyErr, originalKeyErr)
	}
	for ordinal := 0; ordinal < spec.Blocks; ordinal++ {
		block, err := plan.Block(ctx, ordinal)
		want, descriptor := legacyByteStagePGBlock(fixture, ordinal)
		if err != nil || block.Descriptor() != descriptor || !bytes.Equal(block.Bytes(), want) {
			test.Fatal("sealed canonical proof changed an original authenticated byte block", ordinal, err)
		}
	}
	data := work.proofData
	if source, ok := work.canonicalProof.Source().(*legacyProofSpool); !ok || source.legacyDocSpool != data {
		test.Fatal("large header proof did not bind its actual verified output spool")
	}
	if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work == nil || work.work.data != data ||
		work.canonical != nil || work.decoding != nil || work.proofData != nil || work.canonicalProof.Valid() {
		test.Fatal("large header proof did not hand off the same source-backed candidate separately", progress, err)
	}
	progress := outbound.DocRangeWorkPending
	for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && progress != outbound.DocRangeWorkPublishable; quantum++ {
		progress, err = work.Step(ctx, job)
		if err != nil || progress == outbound.DocRangeWorkCompatibilityRequired {
			test.Fatal("large header source-backed candidate failed delegated metadata work", quantum, progress, err)
		}
	}
	if progress != outbound.DocRangeWorkPublishable || generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
		test.Fatal("large header stalled or published its generation before Publish", progress)
	}
	if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
		test.Fatal("private large header candidate advertised public readiness", ready, err)
	}
	if err := work.Publish(ctx, job); err != nil {
		test.Fatal("publish verified large header candidate", err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		reader, err := storage.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
		if err != nil {
			return err
		}
		generation := reader.Descriptor()
		if generation.Version != 5 || generation.Identity() != domain.DocumentIdentityLegacy || generation.ByteStorage() != domain.DocRangeByteStorageDerived ||
			generation.Hash != fixture.hash || generation.EventCount != 2 || !bytes.Equal(generation.Envelope, fixture.envelope) {
			test.Fatal("published large header changed the original document binding or exact envelope")
		}
		stream := docPayloadProjectionPGReadRange(test, storage, read, fixture, reader, 0, len(fixture.stream))
		raw := domain.AssembleCanonicalDoc(generation.Envelope, [][]byte{stream})
		if !bytes.Equal(raw, fixture.raw) || domain.HashContent(raw) != fixture.hash {
			test.Fatal("published large header and authenticated chunks do not reconstruct the original bytes and SHA")
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	var stored []byte
	if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, fixture.hash).Scan(&stored); err != nil || !bytes.Equal(stored, fixture.raw) ||
		immutable != legacyDerivedOriginalImagePG(test, storage, ctx, fixture) || trace.bodyReads.Load() != bodyReads ||
		docRangeJobPGGet(test, storage, ctx, fixture.repo, fixture.hash).Version != job.Version {
		test.Fatal("large header publication reread or changed immutable source bytes/tuples or completed another job", err)
	}
}

func TestDocSourceHeaderPGEachStrictPhaseFencesCurrentAuthority(test *testing.T) {
	for _, phase := range []string{"native", "encoding"} {
		for _, change := range []string{"source version", "source incarnation", "ownership", "snapshot identity", "lease expiry", "job fence", "argument fence", "cancellation"} {
			test.Run(phase+"/"+change, func(test *testing.T) {
				storage, ctx := chunkReusePG(test)
				fixture := docSourceHeaderPGSource(test, storage, ctx, 2*domain.ConversationManifestChunkBytes+73)
				work, job, trace := docSourceHeaderPGStart(test, storage, ctx, fixture)
				before := docSourceHeaderPGPause(test, work, ctx, job, fixture, phase)
				selected, cancel := context.WithCancel(ctx)
				defer cancel()
				candidate := job
				want := error(domain.ErrConflict)
				switch change {
				case "argument fence":
					candidate.Version++
				case "cancellation":
					cancel()
					want = context.Canceled
				default:
					legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
					if change == "ownership" || change == "snapshot identity" {
						want = domain.ErrNotFound
					}
				}
				if change == "source version" || change == "source incarnation" {
					current, _, err := storage.readLegacyCaptureSource(ctx, job, nil, 0, 0)
					if err != nil || current.bytes != len(fixture.raw) ||
						change == "source version" && (current.stage.sourceVersion != work.witness.stage.sourceVersion+1 || current.stage.sourceID != work.witness.stage.sourceID) ||
						change == "source incarnation" && (current.stage.sourceVersion != work.witness.stage.sourceVersion || current.stage.sourceID == work.witness.stage.sourceID) {
						test.Fatal("header fence fixture did not replace its actual current source witness", current, work.witness, err)
					}
				}
				private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
				public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
				immutable := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
				resources, err := work.RetainedResources()
				if err != nil {
					test.Fatal(err)
				}
				queries, bodyReads := trace.queries.Load(), trace.bodyReads.Load()
				progress, err := work.Step(selected, candidate)
				after, actual := docSourceHeaderPGSnapshot(test, work, fixture)
				if !errors.Is(err, want) || progress != 0 || after != before || actual != phase || !reflect.DeepEqual(work.job, job) {
					test.Fatal("revoked strict header quantum advanced its actual candidate, native decoder, encoder, or job", progress, err, want, before, after)
				}
				if after, err := work.RetainedResources(); err != nil || after != resources {
					test.Fatal("revoked strict header quantum changed retained resources", resources, after, err)
				}
				if trace.bodyReads.Load() != bodyReads || (change == "cancellation" || change == "argument fence") && trace.queries.Load() != queries {
					test.Fatal("revoked strict header reread SQL payload or issued SQL before context/argument rejection")
				}
				if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
					test.Fatal("revoked strict header candidate became publishable", err)
				}
				if private != legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) || public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) ||
					immutable != legacyDerivedOriginalImagePG(test, storage, ctx, fixture) || generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
					test.Fatal("revoked strict header changed current persisted source, job, private stage, or public generation")
				}
			})
		}
	}
}

func TestDocSourceHeaderPGNativeReauthenticatesOriginal512KiBPart(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := docSourceHeaderPGSource(test, storage, ctx, 2*domain.LegacyCanonicalEncodingBytes+73)
	work, job, trace := docSourceHeaderPGStart(test, storage, ctx, fixture)
	docSourceHeaderPGPause(test, work, ctx, job, fixture, "native")
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	bodyReads := trace.bodyReads.Load()
	offset := bytes.Index(fixture.raw, []byte(`"source_model":"`)) + len(`"source_model":"`)
	if offset < len(`"source_model":"`) || fixture.raw[offset] != 'm' || offset >= domain.ConversationManifestChunkBytes {
		test.Fatal("fixture did not place its large native scalar in the original first authenticated part")
	}
	if _, err := work.decoding.decoded.file.WriteAt([]byte("n"), int64(offset)); err != nil {
		test.Fatal(err)
	}
	progress, err := work.Step(ctx, job)
	if !errors.Is(err, domain.ErrPreparedDocRangeObject) || progress != 0 || work.canonicalProof.Valid() || work.normalization != nil ||
		work.original || work.work != nil || work.proofData == nil || work.proofData.size != 0 {
		test.Fatal("strict native header trusted its prior hash instead of reauthenticating the original 512KiB part", progress, err)
	}
	if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("damaged original header part exposed publication authority", err)
	}
	if trace.bodyReads.Load() != bodyReads || private != legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) ||
		public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) || generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
		test.Fatal("private header part damage reread or changed the actual stored source, job, or public/private tuples")
	}
}
