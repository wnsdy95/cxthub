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

func legacyHeaderChunkProof(test testing.TB, proof VerifiedSessionDoc, raw []byte) VerifiedSessionDoc {
	test.Helper()
	envelope, stream := chunkVerifierParts(test, raw)
	input := chunkVerifierPlan(envelope, stream, 7)
	var verifier CanonicalDocVerifier
	chunked, err := verifier.VerifyChunks(context.Background(), proof.Hash(), input.Manifest, chunkVerifierLoad(input))
	if err != nil || chunked.chunks == nil || chunked.DocumentRef() != proof.DocumentRef() {
		test.Fatal("invalid chunk-backed legacy fixture", err)
	}
	return chunked
}

func TestLegacyHeaderPreparationActiveCancellation(test *testing.T) {
	proof, raw := legacyDocByteBackfillDocumentFixture(test, CIRDocument{
		Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{strings.Repeat("h", 3*canonicalCheckBytes+17)}}, Events: []CIREvent{},
	})
	chunked := legacyHeaderChunkProof(test, proof, raw)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	for _, operation := range []string{"monolithic-plan", "monolithic-seed", "chunked-seed"} {
		test.Run(operation, func(test *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			checks := 0
			ctx := canonicalAdmissionContext{Context: base, check: func() {
				checks++
				if checks == 5 {
					cancel()
				}
			}}
			if operation == "monolithic-plan" {
				got, err := NewLegacyByteBackfill(ctx, proof)
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, LegacyByteBackfill{}) || checks != 5 {
					test.Fatalf("header preparation ignored active cancellation: checks=%d valid=%t err=%v", checks, got.Valid(), err)
				}
			} else {
				selected := proof
				if operation == "chunked-seed" {
					selected = chunked
				}
				got, err := NewLegacyRangeMetadataSeed(ctx, selected, plan)
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, LegacyRangeMetadataSeed{}) || checks != 5 {
					test.Fatalf("header seed ignored active cancellation: checks=%d valid=%t err=%v", checks, got.valid(), err)
				}
			}
			if !bytes.Equal(proof.Bytes(), raw) || !bytes.Equal(chunked.Bytes(), raw) {
				test.Fatal("cancellation changed the source proofs")
			}
		})
	}
}

func TestLegacyHeaderPreparationExactOwnedEnvelopeAndMetadata(test *testing.T) {
	document := CIRDocument{
		Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{strings.Repeat("x", canonicalCheckBytes-3) + `},"events":[{"escape":"\\"}]` + "\ud55c🙂\u2028\u2029<&>"}},
		Events:   []CIREvent{{Kind: EventMessage, Role: RoleUser, Seq: 7, Blocks: []ContentBlock{{Type: "text", Text: "kept-\ud55c🙂"}}}},
	}
	proof, raw := legacyDocByteBackfillDocumentFixture(test, document)
	header, err := canonicalJSON(document.Envelope)
	if err != nil {
		test.Fatal(err)
	}
	pretty, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		test.Fatal(err)
	}
	if reference, err := VerifyStoredDocBytes(proof.Hash(), pretty); err != nil || reference.DocumentRef() != proof.DocumentRef() {
		test.Fatal("pretty legacy input did not retain its canonical identity", err)
	}
	var normalized CIRDocument
	if err := json.Unmarshal(pretty, &normalized); err != nil {
		test.Fatal(err)
	}
	normalizedProof, err := VerifySessionDoc(SessionDoc{Hash: proof.Hash(), CIR: normalized})
	if err != nil {
		test.Fatal(err)
	}
	chunked := legacyHeaderChunkProof(test, proof, raw)
	var expectedObjects []DocRangeObject
	expected, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(_ context.Context, object DocRangeObject) error {
		expectedObjects = append(expectedObjects, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	var verifier CanonicalDocVerifier
	strictProof, err := verifier.Verify(context.Background(), proof.Hash(), raw)
	if err != nil {
		test.Fatal(err)
	}
	for _, selected := range []VerifiedSessionDoc{proof, strictProof, normalizedProof, chunked} {
		plan, err := NewLegacyByteBackfill(context.Background(), selected)
		if err != nil {
			test.Fatal(err)
		}
		seed, err := NewLegacyRangeMetadataSeed(context.Background(), selected, plan)
		if err != nil || !bytes.Equal(seed.envelope, header) {
			test.Fatal("seed changed the already-verified canonical envelope", err)
		}
		if selected.canonical != "" {
			start := uintptr(unsafe.Pointer(unsafe.StringData(selected.canonical)))
			owned := uintptr(unsafe.Pointer(unsafe.SliceData(seed.envelope)))
			if owned >= start && owned < start+uintptr(len(selected.canonical)) {
				test.Fatal("header seed retained the full transcript backing")
			}
		}
		legacyPreparationRequireReleasedInputs(test, seed, false)
		spooled, source := legacyMetadataSeedSpool(test, plan)
		preparation, err := seed.NewPreparation(context.Background(), spooled)
		if err != nil || len(source.reads) != 0 {
			test.Fatal("header seed construction eagerly read its body spool", err)
		}
		var got VerifiedLegacyDerivedRangeMetadata
		var objects []DocRangeObject
		spec, err := spooled.Spec()
		if err != nil {
			test.Fatal(err)
		}
		for step := 0; step < 2*spec.Blocks+8; step++ {
			complete, err := preparation.Step(context.Background(), func(_ context.Context, object DocRangeObject) error {
				objects = append(objects, DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)})
				return nil
			})
			if err != nil {
				test.Fatal(err)
			}
			if complete {
				got, err = preparation.Metadata()
				if err != nil {
					test.Fatal(err)
				}
				break
			}
		}
		if !got.Valid() {
			test.Fatal("header fixture did not complete within the original step bound")
		}
		if seed.PlanKey() != expected.PlanKey() || !reflect.DeepEqual(got, expected) || !reflect.DeepEqual(objects, expectedObjects) {
			test.Fatal("proof representations changed plan, metadata, hashes or object order")
		}
		if !bytes.Equal(selected.Bytes(), raw) {
			test.Fatal("preparation mutated the original source")
		}
	}
}

func TestLegacyHeaderPreparationCancellationSweep(test *testing.T) {
	proof, raw := legacyDocByteBackfillDocumentFixture(test, CIRDocument{
		Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{strings.Repeat("h", 3*canonicalCheckBytes+17)}}, Events: []CIREvent{},
	})
	chunked := legacyHeaderChunkProof(test, proof, raw)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	for _, operation := range []struct {
		name string
		run  func(context.Context) (bool, bool, error)
	}{
		{"plan", func(ctx context.Context) (bool, bool, error) {
			got, err := NewLegacyByteBackfill(ctx, proof)
			return got.Valid(), reflect.DeepEqual(got, LegacyByteBackfill{}), err
		}},
		{"seed", func(ctx context.Context) (bool, bool, error) {
			got, err := NewLegacyRangeMetadataSeed(ctx, proof, plan)
			return got.valid(), reflect.DeepEqual(got, LegacyRangeMetadataSeed{}), err
		}},
		{"chunk-seed", func(ctx context.Context) (bool, bool, error) {
			got, err := NewLegacyRangeMetadataSeed(ctx, chunked, plan)
			return got.valid(), reflect.DeepEqual(got, LegacyRangeMetadataSeed{}), err
		}},
	} {
		test.Run(operation.name, func(test *testing.T) {
			checkpoints := 0
			probe := canonicalAdmissionContext{Context: context.Background(), check: func() { checkpoints++ }}
			if valid, zero, err := operation.run(probe); err != nil || !valid || zero {
				test.Fatal("uncanceled constructor failed", err)
			}
			for cancelAt := 1; cancelAt <= checkpoints; cancelAt++ {
				base, cancel := context.WithCancel(context.Background())
				checks := 0
				ctx := canonicalAdmissionContext{Context: base, check: func() {
					checks++
					if checks == cancelAt {
						cancel()
					}
				}}
				valid, zero, err := operation.run(ctx)
				cancel()
				if !errors.Is(err, context.Canceled) || valid || !zero || checks != cancelAt {
					test.Fatalf("checkpoint %d/%d leaked a partial constructor result: valid=%t zero=%t checks=%d err=%v", cancelAt, checkpoints, valid, zero, checks, err)
				}
			}
		})
	}
}

func TestLegacyHeaderPreparationOwnsAndAccountsOnlyHeader(test *testing.T) {
	for _, largeHeader := range []bool{false, true} {
		var proof VerifiedSessionDoc
		if largeHeader {
			proof, _ = legacyDocByteBackfillDocumentFixture(test, CIRDocument{
				Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{strings.Repeat("h", 4*canonicalCheckBytes+17)}}, Events: []CIREvent{},
			})
		} else {
			proof, _ = legacyDocByteBackfillSizedFixture(test, 4*ConversationManifestChunkBytes+17)
		}
		plan, err := NewLegacyByteBackfill(context.Background(), proof)
		if err != nil {
			test.Fatal(err)
		}
		seed, err := NewLegacyRangeMetadataSeed(context.Background(), proof, plan)
		if err != nil {
			test.Fatal(err)
		}
		retained, err := seed.RetainedBytes()
		if err != nil || retained < int64(cap(seed.envelope)) || retained > 2*int64(cap(seed.envelope))+2048 || largeHeader && retained < 4*canonicalCheckBytes || !largeHeader && retained >= canonicalCheckBytes {
			test.Fatal("header ownership escaped actual retained-capacity accounting", retained, len(seed.envelope), cap(seed.envelope), err)
		}
		legacyPreparationRequireReleasedInputs(test, seed, false)
	}
}

func TestLegacyHeaderPreparationBoundaryEscapes(test *testing.T) {
	for _, marker := range []string{"🙂", "\\", "\"", `},"events":[`, "[]{}"} {
		for _, delta := range []int{-1, 0, 1} {
			document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{"seam-marker"}}, Events: []CIREvent{}}
			template, err := canonicalJSON(document.Envelope)
			if err != nil {
				test.Fatal(err)
			}
			start := bytes.Index(template, []byte("seam-marker"))
			if start < 0 {
				test.Fatal("header fixture lost its marker")
			}
			document.Envelope.SourceModels[0] = strings.Repeat("x", canonicalCheckBytes+delta-start) + marker + "tail"
			proof, raw := legacyDocByteBackfillDocumentFixture(test, document)
			var verifier CanonicalDocVerifier
			checked, err := verifier.Verify(context.Background(), proof.Hash(), raw)
			if err != nil || !checked.Valid() {
				test.Fatal("byte framing rejected a verified boundary fixture", marker, delta, err)
			}
			plan, err := NewLegacyByteBackfill(context.Background(), checked)
			if err != nil {
				test.Fatal(err)
			}
			seed, err := NewLegacyRangeMetadataSeed(context.Background(), checked, plan)
			want, encodeErr := canonicalJSON(document.Envelope)
			if err != nil || encodeErr != nil || !bytes.Equal(seed.envelope, want) || !bytes.Equal(checked.Bytes(), raw) {
				test.Fatal("header seam changed exact bytes or source ownership", marker, delta, err, encodeErr)
			}
		}
	}
}
