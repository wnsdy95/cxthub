package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyRangeMetadataExternalEnvelopeUsesBoundedPreparation(test *testing.T) {
	proof, raw := legacyDocByteBackfillDocumentFixture(test, CIRDocument{
		Envelope: CIREnvelope{CIRVersion: CIRVersionV2, SourceProvider: ProviderCodex, Fidelity: FidelityFull,
			Cwd: strings.Repeat("header🙂", MaxConversationManifestBytes/4)},
		Events: []CIREvent{{Kind: EventMessage, Seq: 1, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "body stays separate"}}}},
	})
	envelope, _ := chunkVerifierParts(test, raw)
	if len(envelope) <= MaxConversationManifestBytes {
		test.Fatal("fixture did not cross the inline header limit")
	}
	state, plan, source := legacyMetadataWorkFixture(test, proof)
	if state.generation.Version != 6 || state.generation.EnvelopeRef == nil {
		test.Fatal("large header did not select its explicit reference format")
	}
	var objects []DocRangeObject
	save := func(_ context.Context, object DocRangeObject) error {
		objects = append(objects, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
		return nil
	}
	for step := 0; state.generation.Envelope != nil; step++ {
		before := state.envelopePosition
		complete, err := state.Step(context.Background(), save)
		if err != nil || complete || len(source.reads) != 0 || len(objects) != 0 ||
			state.generation.Envelope != nil && state.envelopePosition-before > LegacyCanonicalEncodingBytes {
			test.Fatal("header preparation read the body, emitted duplicate header objects, or exceeded its quantum", step, complete, err)
		}
		if step > (len(envelope)+LegacyCanonicalEncodingBytes-1)/LegacyCanonicalEncodingBytes {
			test.Fatal("header preparation did not finish")
		}
	}
	if state.generation.EnvelopeRef.Hash != HashContent(envelope) || state.generation.EnvelopeRef.Bytes != len(envelope) ||
		state.envelopePosition != 0 || len(state.envelopeHashState) != 0 {
		test.Fatal("header reference changed canonical bytes or retained the consumed header state")
	}
	for step := 0; step < 100; step++ {
		complete, err := state.Step(context.Background(), save)
		if err != nil {
			test.Fatal(err)
		}
		if !complete {
			continue
		}
		metadata, err := state.Metadata()
		if err != nil {
			test.Fatal(err)
		}
		generation, err := metadata.Descriptor()
		if err != nil || generation.Version != 6 || generation.ByteStorage() != DocRangeByteStorageDerived ||
			generation.Envelope != nil || generation.EnvelopeRef.Hash != HashContent(envelope) {
			test.Fatal("large header could not seal a bounded derived generation", err)
		}
		encoded, err := generation.Encode()
		if err != nil || len(encoded) > MaxDocRangeGenerationBytes || len(encoded) >= len(envelope) {
			test.Fatal("descriptor retained the full large header", len(encoded), err)
		}
		decoded, err := DecodeDocRangeGeneration(proof.Hash(), HashContent(encoded), encoded)
		if err != nil || !reflect.DeepEqual(generation, decoded) {
			test.Fatal("reference format did not round-trip canonically", err)
		}
		var bulkObjects []DocRangeObject
		bulk, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(_ context.Context, object DocRangeObject) error {
			bulkObjects = append(bulkObjects, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
			return nil
		})
		if err != nil || !reflect.DeepEqual(metadata, bulk) || !reflect.DeepEqual(objects, bulkObjects) {
			test.Fatal("bulk and worker paths interpreted the large-header contract differently", err)
		}
		if key, err := plan.Key(); err != nil || key != metadata.PlanKey() {
			test.Fatal("header externalization changed the original byte plan", err)
		}
		return
	}
	test.Fatal("large-header metadata did not finish")
}

func TestDocRangeExternalEnvelopeRejectsInvalidReferenceShapes(test *testing.T) {
	identity := DocumentIdentityLegacy
	generation := DocRangeGeneration{Version: 6, SourceIdentity: &identity, Algorithm: 1,
		Hash: HashContent([]byte("original document")), EnvelopeRef: &DocRangeEnvelopeReference{
			Hash: HashContent([]byte("header")), Bytes: MaxConversationManifestBytes + 1}}
	if err := generation.Validate(generation.Hash); err != nil {
		test.Fatal(err)
	}
	for _, change := range []string{"inline", "small", "huge", "hash", "old-version", "identity", "missing"} {
		test.Run(change, func(test *testing.T) {
			candidate := generation
			reference := *generation.EnvelopeRef
			candidate.EnvelopeRef = &reference
			switch change {
			case "inline":
				candidate.Envelope = []byte(`{"cir_version":"2"}`)
			case "small":
				candidate.EnvelopeRef.Bytes = MaxConversationManifestBytes
			case "huge":
				candidate.EnvelopeRef.Bytes = MaxConversationDocumentBytes
			case "hash":
				candidate.EnvelopeRef.Hash = "invalid"
			case "old-version":
				candidate.Version = 5
			case "identity":
				candidate.SourceIdentity = nil
			case "missing":
				candidate.EnvelopeRef = nil
			}
			if err := candidate.Validate(candidate.Hash); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid envelope reference was accepted", change, err)
			}
		})
	}
	verified := VerifiedDocRangeGeneration{generation: generation}
	copy := verified.Descriptor()
	copy.EnvelopeRef.Hash = HashContent([]byte("different header"))
	if verified.Descriptor().EnvelopeRef.Hash != generation.EnvelopeRef.Hash {
		test.Fatal("descriptor exposed a mutable envelope reference")
	}
}

func TestLegacyRangeMetadataExternalEnvelopeCancellationPreservesProgress(test *testing.T) {
	proof, _ := legacyDocByteBackfillDocumentFixture(test, CIRDocument{Envelope: CIREnvelope{CIRVersion: CIRVersionV2,
		Cwd: strings.Repeat("header", MaxConversationManifestBytes/3)}})
	state, _, _ := legacyMetadataWorkFixture(test, proof)
	if complete, err := state.Step(context.Background(), func(context.Context, DocRangeObject) error { return nil }); err != nil || complete {
		test.Fatal(err)
	}
	before := state.envelopePosition
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if complete, err := state.Step(ctx, func(context.Context, DocRangeObject) error { return nil }); complete || !errors.Is(err, context.Canceled) || state.envelopePosition != before {
		test.Fatal("canceled header hashing consumed more input", complete, err)
	}
}
