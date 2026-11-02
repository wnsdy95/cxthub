package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyMetadataSeedSpool(test testing.TB, plan LegacyByteBackfill) (LegacyByteBackfill, *legacyMetadataWorkStream) {
	test.Helper()
	source := &legacyMetadataWorkStream{legacyTestStream: &legacyTestStream{}, test: test}
	preparation, err := NewLegacyByteSpoolPreparation(context.Background(), plan, source)
	if err != nil {
		test.Fatal(err)
	}
	spooled := legacySpoolWorkFinish(test, preparation, plan.spec.Blocks)
	source.reads = nil
	return spooled, source
}

func legacyMetadataSeedRootProof(test testing.TB, raw []byte) VerifiedSessionDoc {
	test.Helper()
	var document CIRDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		test.Fatal(err)
	}
	manifest, bodies, err := ConversationManifestForCIR(document)
	if err != nil {
		test.Fatal(err)
	}
	var verifier CanonicalDocVerifier
	proof, err := verifier.VerifyConversationManifestDoc(context.Background(), manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies))
	if err != nil {
		test.Fatal(err)
	}
	return proof
}

func TestLegacyRangeMetadataSeedOriginalPlanToFrozenMetadataParity(test *testing.T) {
	for _, fixture := range []struct {
		name        string
		count       int
		fingerprint ContentHash
	}{
		{"empty", 0, "sha256:1840b03939874ad97dc2582d30eca4e5093ef43ac0d7bee3ccb636f56a0217ce"},
		{"event-pages", DocRangePageEvents + 3, "sha256:1ce99541a5b530826077824e5b6c94e441e022d0c47266d461159c6a68273d17"},
		{"byte-blocks", 0, "sha256:2537c93cb7c0b4406bad846095e95e0ee62fb1be2a5068a6e5ee017eede1ffa6"},
		{"bounded-many-small-events", DocRangePageEvents*DocRangeDirectoryFanout + 1, "sha256:68a0569ce0495f9b4e028da66b54a62315fd379afa0a3852306683fa5c315d5b"},
		{"split-utf8", 0, ""}, {"split-escape", 0, ""}, {"callback-mutation", DocRangePageEvents + 3, ""},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var proof VerifiedSessionDoc
			switch fixture.name {
			case "byte-blocks":
				proof, _ = legacyDocByteGenerationLargeFixture(test)
			case "split-utf8":
				proof, _ = legacyDocByteBackfillBoundaryFixture(test, "\ud55c🙂")
			case "split-escape":
				proof, _ = legacyDocByteBackfillBoundaryFixture(test, "\\\"\n\t")
			default:
				proof, _ = legacyDocRangeGenerationFixture(test, fixture.count)
			}
			plan, err := NewLegacyByteBackfill(context.Background(), proof)
			if err != nil {
				test.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			seed, err := NewLegacyRangeMetadataSeed(ctx, proof, plan)
			cancel()
			if err != nil || seed.Reference() != proof.DocumentRef() || seed.PlanKey() != plan.key {
				test.Fatal("original sealed unspooled plan could not mint a bound seed", err)
			}
			legacyPreparationRequireReleasedInputs(test, seed, false)
			if retained, err := seed.RetainedBytes(); err != nil || retained <= 0 || retained >= 64<<10 {
				test.Fatal("metadata seed retained the proof body or event plan", retained, err)
			}
			var expected []DocRangeObject
			want, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(_ context.Context, object DocRangeObject) error {
				expected = append(expected, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
				return nil
			})
			if err != nil {
				test.Fatal(err)
			}
			spooled, source := legacyMetadataSeedSpool(test, plan)
			ctx, cancel = context.WithCancel(context.Background())
			state, err := seed.NewPreparation(ctx, spooled)
			cancel()
			if err != nil || state == nil || state.Reference() != seed.Reference() || state.PlanKey() != seed.PlanKey() || state.ObjectCount() != 0 || len(source.reads) != 0 {
				test.Fatal("seed preparation scanned the spool or changed bound identity", err)
			}
			got, saved := legacyMetadataWorkFinish(test, state, spooled, source, fixture.name == "callback-mutation", fixture.name == "bounded-many-small-events")
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(saved, expected) {
				test.Fatal("seed path changed the old final metadata capsule, object order, or exact bytes")
			}
			fingerprint := struct {
				Descriptor json.RawMessage `json:"descriptor"`
				Objects    []ContentHash   `json:"objects"`
			}{Descriptor: got.descriptor, Objects: make([]ContentHash, 0, len(saved))}
			for _, object := range saved {
				fingerprint.Objects = append(fingerprint.Objects, object.Hash)
			}
			raw, err := json.Marshal(fingerprint)
			if err != nil || fixture.fingerprint != "" && HashContent(raw) != fixture.fingerprint {
				test.Fatal("seed path changed the frozen metadata fingerprint", HashContent(raw), err)
			}
		})
	}
}

func TestLegacyRangeMetadataSeedOwnsHeaderReferenceAndKeyWithoutProofBody(test *testing.T) {
	proof, raw := legacyDocByteBackfillSizedFixture(test, 4*ConversationManifestChunkBytes+17)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	hashBacking := string(proof.hash) + strings.Repeat("h", len(raw))
	keyBacking := string(plan.key) + strings.Repeat("k", len(raw))
	proof.hash = ContentHash(hashBacking[:len(proof.hash)])
	plan.spec.Reference.Hash = proof.hash
	plan.key = ContentHash(keyBacking[:len(plan.key)])
	ctx, cancel := context.WithCancel(context.Background())
	seed, err := NewLegacyRangeMetadataSeed(ctx, proof, plan)
	cancel()
	if err != nil || seed.Reference() != proof.DocumentRef() || seed.PlanKey() != plan.key {
		test.Fatal(err)
	}
	if unsafe.StringData(string(seed.Reference().Hash)) == unsafe.StringData(string(proof.hash)) || unsafe.StringData(string(seed.PlanKey())) == unsafe.StringData(string(plan.key)) {
		test.Fatal("metadata seed retained transcript-sized reference or key backing")
	}
	legacyPreparationRequireReleasedInputs(test, seed, false)
	retained, err := seed.RetainedBytes()
	if err != nil || retained <= 0 || retained >= 64<<10 {
		test.Fatal("small-header seed accounting scaled with transcript bytes", retained, err)
	}
	spooled, source := legacyMetadataSeedSpool(test, plan)
	fromSpooled, err := NewLegacyRangeMetadataSeed(context.Background(), proof, spooled)
	if err != nil || fromSpooled.Reference() != seed.Reference() || fromSpooled.PlanKey() != seed.PlanKey() || len(source.reads) != 0 {
		test.Fatal("matching source-backed plan could not mint the same seed without reads", err)
	}
	legacyPreparationRequireReleasedInputs(test, fromSpooled, false)
	first, err := seed.NewPreparation(context.Background(), spooled)
	if err != nil {
		test.Fatal(err)
	}
	second, err := seed.NewPreparation(context.Background(), spooled)
	if err != nil {
		test.Fatal(err)
	}
	if len(source.reads) != 0 || !bytes.Equal(first.generation.Envelope, second.generation.Envelope) {
		test.Fatal("seed preparations changed header bytes or eagerly read the source")
	}
	got, _ := legacyMetadataWorkFinish(test, first, spooled, source, false, false)
	descriptor, err := got.Descriptor()
	if err != nil {
		test.Fatal(err)
	}
	descriptor.Envelope[0] ^= 1
	third, err := fromSpooled.NewPreparation(context.Background(), spooled)
	if err != nil || !bytes.Equal(third.generation.Envelope, second.generation.Envelope) {
		test.Fatal("mutating a returned descriptor header changed the reusable seed", err)
	}
	repeated, _ := legacyMetadataWorkFinish(test, second, spooled, source, false, false)
	if !got.Valid() || !reflect.DeepEqual(got, repeated) || got.Reference() != proof.DocumentRef() || !bytes.Equal(proof.Bytes(), raw) {
		test.Fatal("seed/header ownership changed its immutable source or final capsule")
	}
	source.body = append(source.body, bytes.Repeat([]byte("source-owned"), ConversationManifestChunkBytes)...)
	if after, err := seed.RetainedBytes(); err != nil || after != retained {
		test.Fatal("metadata seed accounting included external stream storage", after, err)
	}
}

func TestLegacyRangeMetadataSeedRetainedAccountingIncludesOwnedHeader(test *testing.T) {
	var retained [2]int64
	var headerBytes [2]int
	for position, size := range []int{1, 16 << 10} {
		proof, raw := legacyDocByteBackfillDocumentFixture(test, CIRDocument{
			Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{strings.Repeat("h", size)}},
			Events:   []CIREvent{},
		})
		plan, err := NewLegacyByteBackfill(context.Background(), proof)
		if err != nil {
			test.Fatal(err)
		}
		seed, err := NewLegacyRangeMetadataSeed(context.Background(), proof, plan)
		if err != nil {
			test.Fatal(err)
		}
		header, _ := chunkVerifierParts(test, raw)
		headerBytes[position] = len(header)
		retained[position], err = seed.RetainedBytes()
		if err != nil || retained[position] < int64(len(header)) || retained[position] > int64(4*len(header)+(16<<10)) {
			test.Fatal("metadata seed omitted its owned header or retained unrelated backing", retained[position], len(header), err)
		}
		legacyPreparationRequireReleasedInputs(test, seed, false)
	}
	if retained[1]-retained[0] < int64(headerBytes[1]-headerBytes[0]) {
		test.Fatal("header growth escaped seed retained-byte accounting", retained, headerBytes)
	}
}

func TestLegacyRangeMetadataSeedRejectsUnboundAndSerializedInputs(test *testing.T) {
	proof, raw := legacyDocRangeGenerationFixture(test, 1)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	spooled, source := legacyMetadataSeedSpool(test, plan)
	other, _ := legacyDocRangeGenerationFixture(test, 2)
	root := legacyMetadataSeedRootProof(test, raw)
	unsealed, wrongSize := plan, plan
	unsealed.sealed = false
	wrongSize.spec.StreamBytes--
	wrongSize.stream.size--
	wrongSizeSpec, err := wrongSize.spec.Encode()
	if err != nil {
		test.Fatal(err)
	}
	wrongSize.key = HashContent(wrongSizeSpec)
	encoded, err := json.Marshal(plan)
	if err != nil {
		test.Fatal(err)
	}
	var decodedPlan LegacyByteBackfill
	if err := json.Unmarshal(encoded, &decodedPlan); err != nil {
		test.Fatal(err)
	}
	encoded, err = json.Marshal(proof)
	if err != nil {
		test.Fatal(err)
	}
	var decodedProof VerifiedSessionDoc
	if err := json.Unmarshal(encoded, &decodedProof); err != nil {
		test.Fatal(err)
	}
	for name, candidate := range map[string]struct {
		proof VerifiedSessionDoc
		plan  LegacyByteBackfill
		want  error
	}{
		"zero":                  {want: ErrIntegrity},
		"zero-proof":            {plan: plan, want: ErrIntegrity},
		"zero-plan":             {proof: proof, want: ErrIntegrity},
		"serialized-proof":      {proof: decodedProof, plan: plan, want: ErrIntegrity},
		"serialized-plan":       {proof: proof, plan: decodedPlan, want: ErrIntegrity},
		"unsealed-plan":         {proof: proof, plan: unsealed, want: ErrIntegrity},
		"different-reference":   {proof: other, plan: plan, want: ErrIntegrity},
		"different-stream-size": {proof: proof, plan: wrongSize, want: ErrIntegrity},
		"root-proof":            {proof: root, plan: spooled, want: ErrUnsupportedDocumentIdentity},
	} {
		test.Run(name, func(test *testing.T) {
			seed, err := NewLegacyRangeMetadataSeed(context.Background(), candidate.proof, candidate.plan)
			if !errors.Is(err, candidate.want) || !reflect.DeepEqual(seed, LegacyRangeMetadataSeed{}) || len(source.reads) != 0 {
				test.Fatal("unbound input minted a metadata seed or reached source bytes", err)
			}
		})
	}
	seed, err := NewLegacyRangeMetadataSeed(context.Background(), proof, plan)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err = json.Marshal(seed)
	if err != nil {
		test.Fatal(err)
	}
	var decodedSeed LegacyRangeMetadataSeed
	if err := json.Unmarshal(encoded, &decodedSeed); err != nil {
		test.Fatal(err)
	}
	for _, invalid := range []LegacyRangeMetadataSeed{{}, decodedSeed} {
		if invalid.Reference() != (DocumentRef{}) || invalid.PlanKey() != "" {
			test.Fatal("zero or serialized seed exposed trusted identity")
		}
		if retained, err := invalid.RetainedBytes(); retained != 0 || !errors.Is(err, ErrIntegrity) {
			test.Fatal("zero or serialized seed exposed trusted accounting", retained, err)
		}
		if state, err := invalid.NewPreparation(context.Background(), spooled); state != nil || !errors.Is(err, ErrIntegrity) || len(source.reads) != 0 {
			test.Fatal("zero or serialized seed minted metadata work", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if canceledSeed, err := NewLegacyRangeMetadataSeed(ctx, proof, plan); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(canceledSeed, LegacyRangeMetadataSeed{}) {
		test.Fatal("pre-canceled seed construction retained a partial seed", err)
	}
	if state, err := seed.NewPreparation(ctx, spooled); !errors.Is(err, context.Canceled) || state != nil || len(source.reads) != 0 {
		test.Fatal("pre-canceled seed preparation read the spool or acquired work", err)
	}
	if state, err := NewLegacyRangeMetadataPreparation(context.Background(), proof, plan); !errors.Is(err, ErrIntegrity) || state != nil {
		test.Fatal("old constructor started accepting unspooled plans", err)
	}
	state, err := seed.NewPreparation(context.Background(), spooled)
	if err != nil || state == nil || len(source.reads) != 0 {
		test.Fatal("pre-cancellation poisoned the reusable seed", err)
	}
}

func TestLegacyRangeMetadataSeedPreparationRequiresMatchingSealedSpool(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, 1)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	seed, err := NewLegacyRangeMetadataSeed(context.Background(), proof, plan)
	if err != nil {
		test.Fatal(err)
	}
	spooled, source := legacyMetadataSeedSpool(test, plan)
	unsealed, wrongKey, wrongReference, wrongSize, wrongDescriptor := spooled, spooled, spooled, spooled, spooled
	unsealed.sealed = false
	wrongKey.key = HashContent([]byte("different spool plan key"))
	wrongReference.spec.Reference.Hash = HashContent([]byte("different spool document"))
	wrongSize.spec.StreamBytes--
	wrongDescriptor.descriptors = append([]ConversationManifestChunk(nil), spooled.descriptors...)
	wrongDescriptor.descriptors[0].Bytes--
	encoded, err := json.Marshal(spooled)
	if err != nil {
		test.Fatal(err)
	}
	var decoded LegacyByteBackfill
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		test.Fatal(err)
	}
	for name, candidate := range map[string]LegacyByteBackfill{
		"zero": {}, "original-unspooled": plan, "unsealed": unsealed, "serialized": decoded,
		"plan-key": wrongKey, "reference": wrongReference, "stream-size": wrongSize, "descriptor-size": wrongDescriptor,
	} {
		test.Run(name, func(test *testing.T) {
			state, err := seed.NewPreparation(context.Background(), candidate)
			if state != nil || !errors.Is(err, ErrIntegrity) || len(source.reads) != 0 {
				test.Fatal("seed accepted an unsealed, unspooled, or mismatched plan", err)
			}
		})
	}
	if state, err := seed.NewPreparation(context.Background(), spooled); state == nil || err != nil || len(source.reads) != 0 {
		test.Fatal("rejected plans poisoned the reusable seed", err)
	}
}

func TestLegacyRangeMetadataSeedRemainsReusableAfterPreparationFailure(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, DocRangePageEvents+1)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	seed, err := NewLegacyRangeMetadataSeed(context.Background(), proof, plan)
	if err != nil {
		test.Fatal(err)
	}
	retained, err := seed.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	for _, fault := range []string{"read-error", "read-cancel", "source-corruption", "save-error", "save-cancel"} {
		test.Run(fault, func(test *testing.T) {
			spooled, source := legacyMetadataSeedSpool(test, plan)
			state, err := seed.NewPreparation(context.Background(), spooled)
			if err != nil {
				test.Fatal(err)
			}
			original := bytes.Clone(source.body)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source.ctx = ctx
			failure := errors.New("seed metadata callback failed")
			want := failure
			switch fault {
			case "read-error":
				source.readError = failure
			case "read-cancel":
				source.readTransform = func(body []byte) []byte { cancel(); return body }
				want = context.Canceled
			case "source-corruption":
				source.body[0] ^= 1
				want = ErrPreparedDocRangeObject
			case "save-cancel":
				want = context.Canceled
			}
			complete, err := state.Step(ctx, func(saveCtx context.Context, object DocRangeObject) error {
				if saveCtx != ctx || HashContent(object.Bytes) != object.Hash {
					test.Fatal("seed work lost callback context or authenticated object bytes")
				}
				legacyMetadataWorkPending(test, state)
				if fault == "save-error" {
					return failure
				}
				if fault == "save-cancel" {
					cancel()
				}
				return nil
			})
			if complete || !errors.Is(err, want) {
				test.Fatal("seed preparation failure completed or lost its cause", err)
			}
			metadata, metadataError := state.Metadata()
			if !errors.Is(metadataError, ErrIntegrity) || !reflect.DeepEqual(metadata, VerifiedLegacyDerivedRangeMetadata{}) {
				test.Fatal("failed seed preparation exposed partial metadata", metadataError)
			}
			source.body, source.readError, source.readTransform, source.ctx = original, nil, nil, context.Background()
			reads := len(source.reads)
			complete, err = state.Step(source.ctx, func(context.Context, DocRangeObject) error {
				test.Fatal("poisoned seed preparation resumed callbacks")
				return nil
			})
			if complete || err == nil || len(source.reads) != reads {
				test.Fatal("repairing storage revived poisoned seed preparation", err)
			}
			source.reads, source.ctx = nil, nil
			fresh, err := seed.NewPreparation(context.Background(), spooled)
			if err != nil || fresh == nil || len(source.reads) != 0 {
				test.Fatal("failed preparation poisoned its reusable seed", err)
			}
			got, _ := legacyMetadataWorkFinish(test, fresh, spooled, source, false, true)
			if !got.Valid() || got.Reference() != seed.Reference() || got.PlanKey() != seed.PlanKey() {
				test.Fatal("fresh seed preparation lost its binding after an earlier failure")
			}
			if after, err := seed.RetainedBytes(); err != nil || after != retained {
				test.Fatal("preparation failure or retry changed seed retention", after, err)
			}
		})
	}
}
