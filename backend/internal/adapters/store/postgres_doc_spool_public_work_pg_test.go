//go:build postgres

package store

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docSpoolQuantumPGProofState struct {
	phase                                    uint64
	position, headerCopied, copied, verified int64
	headerBytes                              int
	envelope                                 bool
	strict                                   docSourceHeaderPGState
}

func docSpoolQuantumPGProofSnapshot(test *testing.T, capture *preparedLegacyCaptureWork, fixture legacyByteStagePGFixture) docSpoolQuantumPGProofState {
	test.Helper()
	if capture.canonical == nil {
		return docSpoolQuantumPGProofState{}
	}
	preparation := reflect.ValueOf(capture.canonical).Elem()
	state := docSpoolQuantumPGProofState{
		phase: preparation.FieldByName("phase").Uint(), position: preparation.FieldByName("position").Int(),
		headerCopied: preparation.FieldByName("headerCopied").Int(), headerBytes: preparation.FieldByName("header").Len(),
		copied: preparation.FieldByName("copied").Int(), verified: preparation.FieldByName("verified").Int(),
		envelope: !preparation.FieldByName("envelope").IsNil(),
	}
	if state.envelope {
		state.strict, _ = docSourceHeaderPGSnapshot(test, capture, fixture)
		state.strict.preparation, state.strict.envelope, state.strict.native, state.strict.encoding, state.strict.sourceParts = 0, 0, 0, 0, 0
	}
	return state
}

func docSpoolQuantumPGProofPhase(test *testing.T, state docSpoolQuantumPGProofState) string {
	test.Helper()
	switch state.phase {
	case 0:
		return "hash"
	case 1:
		if !state.envelope {
			return "decision"
		}
		switch state.strict.phase {
		case 0:
			return "native"
		case 1:
			return "initialize"
		case 2:
			return "encoding"
		case 3:
			return "strict-eof"
		}
	case 5:
		if state.headerBytes == 0 {
			return "seed-allocate"
		}
		if state.headerCopied < int64(state.headerBytes) {
			return "seed-copy"
		}
		return "seed-eof"
	case 2:
		return "copy"
	case 3:
		return "verify"
	case 4:
		return "seal"
	}
	test.Fatal("unexpected canonical proof phase", state)
	return ""
}

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
	phases := []string{"hash", "decision", "native", "initialize", "encoding", "strict-eof", "seed-allocate", "seed-copy", "seed-eof", "copy", "verify", "seal"}
	counts := make(map[string]int, len(phases))
	seen := make(map[docSpoolQuantumPGProofState]bool)
	phaseIndex := 0
	for step := 0; step < docCaptureWorkPGPreparationStepLimit && !capture.canonicalProof.Valid(); step++ {
		before := docSpoolQuantumPGProofSnapshot(test, capture, fixture)
		phase := docSpoolQuantumPGProofPhase(test, before)
		if phase != phases[phaseIndex] {
			phaseIndex++
			if phaseIndex >= len(phases) || phase != phases[phaseIndex] {
				test.Fatal("canonical proof skipped or revisited a distinct phase", phase, counts)
			}
		}
		if seen[before] {
			test.Fatal("canonical proof repeated semantic state without bounded progress", phase, before)
		}
		seen[before] = true
		counts[phase]++
		authorityChecks := trace.authorityChecks.Load()
		progress, err := prepared.Step(ctx, job)
		if err != nil || progress != outbound.DocRangeWorkPending || capture.work != nil || capture.original ||
			capture.normalization != nil || capture.canonicalProof.Valid() != (phase == "seal") || trace.authorityChecks.Load() != authorityChecks+1 {
			test.Fatal("canonical proof skipped a yield or current source/ownership/job confirmation", phase, progress, err)
		}
		after := docSpoolQuantumPGProofSnapshot(test, capture, fixture)
		wantPosition, wantHeaderCopied, wantCopied, wantVerified := before.position, before.headerCopied, before.copied, before.verified
		switch phase {
		case "hash":
			wantPosition = min(before.position+domain.ConversationManifestChunkBytes, int64(len(fixture.raw)))
		case "seed-copy":
			wantHeaderCopied = min(before.headerCopied+domain.LegacyCanonicalEncodingBytes, int64(len(fixture.envelope)))
		case "copy":
			wantCopied++
		case "verify":
			wantVerified++
		}
		if after.position != wantPosition || after.headerCopied != wantHeaderCopied || after.copied != wantCopied || after.verified != wantVerified ||
			after == before && phase != "seal" {
			test.Fatal("canonical quantum copied, read back, or advanced beyond its own phase", phase, before, after)
		}
		if phaseIndex < 6 && after.headerBytes != 0 || phaseIndex >= 6 && after.headerBytes != len(fixture.envelope) {
			test.Fatal("canonical header allocated before strict verification or changed its extent", phase, after.headerBytes)
		}
		if after.headerCopied != 0 && !bytes.Equal(reflect.ValueOf(capture.canonical).Elem().FieldByName("header").Bytes()[:after.headerCopied], fixture.envelope[:after.headerCopied]) {
			test.Fatal("bounded seed copy changed original header bytes", phase, after.headerCopied)
		}
		if phase != "seal" && reflect.ValueOf(&peer.docProofs.rootVerifier).Elem().FieldByName("events").Len() != 0 {
			test.Fatal("provisional canonical phase accepted event proof authority", phase)
		}
		expectedBytes := min(counts["copy"]*domain.ConversationManifestChunkBytes, len(fixture.stream))
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
	if !capture.canonicalProof.Valid() || counts["hash"] != inputQuanta || counts["decision"] != 1 || counts["native"] == 0 ||
		counts["initialize"] != 1 || counts["encoding"] == 0 || counts["strict-eof"] != 1 || counts["seed-allocate"] != 1 ||
		counts["seed-copy"] != (len(fixture.envelope)+domain.LegacyCanonicalEncodingBytes-1)/domain.LegacyCanonicalEncodingBytes ||
		counts["seed-eof"] != 1 || counts["copy"] != outputQuanta || counts["verify"] != outputQuanta || counts["seal"] != 1 {
		test.Fatal("canonical proof lost its original byte quanta or distinct bounded header phases", counts)
	}
	test.Log("canonical proof phase progress", counts)
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
