package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func legacyDocByteBackfillDocumentFixture(test testing.TB, document CIRDocument) (VerifiedSessionDoc, []byte) {
	test.Helper()
	raw, err := CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	proof, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: document})
	if err != nil || !proof.Valid() || proof.DocumentRef().Identity != DocumentIdentityLegacy {
		test.Fatalf("legacy byte backfill fixture verification: %v", err)
	}
	return proof, raw
}

func legacyDocByteBackfillSizedFixture(test testing.TB, streamBytes int) (VerifiedSessionDoc, []byte) {
	test.Helper()
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{"envelope-only-backfill-model"}}, Events: []CIREvent{
		{Kind: EventMessage, Seq: 17, Role: RoleAssistant, Blocks: []ContentBlock{{Type: "text"}}},
	}}
	_, raw := legacyDocByteBackfillDocumentFixture(test, document)
	_, stream := chunkVerifierParts(test, raw)
	if streamBytes < len(stream) {
		test.Fatalf("requested stream is shorter than the empty event: %d < %d", streamBytes, len(stream))
	}
	document.Events[0].Blocks[0].Text = strings.Repeat("x", streamBytes-len(stream))
	proof, raw := legacyDocByteBackfillDocumentFixture(test, document)
	_, stream = chunkVerifierParts(test, raw)
	if len(stream) != streamBytes {
		test.Fatalf("sized backfill fixture has %d stream bytes, want %d", len(stream), streamBytes)
	}
	return proof, raw
}

func legacyDocByteBackfillBoundaryFixture(test testing.TB, marker string) (VerifiedSessionDoc, []byte) {
	test.Helper()
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{"envelope-only-backfill-model"}}, Events: []CIREvent{
		{Kind: EventMessage, Seq: 17, Role: RoleAssistant, Blocks: []ContentBlock{{Type: "text", Text: marker + "tail"}}},
	}}
	_, raw := legacyDocByteBackfillDocumentFixture(test, document)
	_, stream := chunkVerifierParts(test, raw)
	encoded, err := json.Marshal(marker)
	if err != nil {
		test.Fatal(err)
	}
	encoded = encoded[1 : len(encoded)-1]
	position := bytes.Index(stream, encoded)
	if position < 0 || position >= ChunkTarget-1 {
		test.Fatalf("missing boundary marker at a usable offset: %d", position)
	}
	document.Events[0].Blocks[0].Text = strings.Repeat("x", ChunkTarget-1-position) + marker + "tail"
	proof, raw := legacyDocByteBackfillDocumentFixture(test, document)
	_, stream = chunkVerifierParts(test, raw)
	if bytes.Index(stream, encoded) != ChunkTarget-1 {
		test.Fatal("fixture does not split the UTF-8 code point or JSON escape at the block boundary")
	}
	return proof, raw
}

func legacyDocByteBackfillRequirePlan(test testing.TB, proof VerifiedSessionDoc, stream []byte) (LegacyByteBackfill, DocLegacyByteBackfillSpec, ContentHash) {
	test.Helper()
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil || !plan.Valid() {
		test.Fatalf("construct legacy byte backfill: %v", err)
	}
	spec, err := plan.Spec()
	want := DocLegacyByteBackfillSpec{
		Version: 1, Reference: proof.DocumentRef(), StreamBytes: int64(len(stream)),
		BlockBytes: ChunkTarget, Blocks: (len(stream) + ChunkTarget - 1) / ChunkTarget,
	}
	if err != nil || spec != want || spec.Validate() != nil {
		test.Fatalf("backfill spec: %+v, %v; want %+v", spec, err, want)
	}
	encoded, err := spec.Encode()
	if err != nil {
		test.Fatal(err)
	}
	canonical, err := canonicalJSON(spec)
	if err != nil || !bytes.Equal(encoded, canonical) {
		test.Fatalf("backfill spec is not canonical JSON: %v", err)
	}
	key, err := plan.Key()
	if err != nil || ValidateContentHash(key) != nil || key != HashContent(encoded) {
		test.Fatalf("backfill key does not commit the exact encoded spec: %s, %v", key, err)
	}
	decoded, err := DecodeDocLegacyByteBackfillSpec(key, encoded)
	if err != nil || decoded != spec {
		test.Fatalf("backfill spec round trip: %+v, %v", decoded, err)
	}
	return plan, spec, key
}

func legacyDocByteBackfillRequireBlock(test testing.TB, block VerifiedLegacyByteBlock, spec DocLegacyByteBackfillSpec, key ContentHash, ordinal int, stream []byte) {
	test.Helper()
	start := ordinal * ChunkTarget
	want := stream[start:min(start+ChunkTarget, len(stream))]
	descriptor := ConversationManifestChunk{Hash: HashContent(want), Bytes: int64(len(want))}
	if !block.Valid() || block.Reference() != spec.Reference || block.PlanKey() != key || block.Ordinal() != ordinal ||
		block.Descriptor() != descriptor || !bytes.Equal(block.Bytes(), want) {
		test.Fatalf("block %d changed source identity, plan, ordinal, descriptor, or exact canonical stream bytes", ordinal)
	}
}

func TestLegacyByteBackfillCanonicalStreamBoundariesAndSeeking(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		build func(testing.TB) (VerifiedSessionDoc, []byte)
	}{
		{name: "empty", build: func(test testing.TB) (VerifiedSessionDoc, []byte) { return legacyDocRangeGenerationFixture(test, 0) }},
		{name: "multiple-events", build: func(test testing.TB) (VerifiedSessionDoc, []byte) { return legacyDocRangeGenerationFixture(test, 9) }},
		{name: "boundary-minus-one", build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocByteBackfillSizedFixture(test, ChunkTarget-1)
		}},
		{name: "exact-boundary", build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocByteBackfillSizedFixture(test, ChunkTarget)
		}},
		{name: "boundary-plus-one", build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocByteBackfillSizedFixture(test, ChunkTarget+1)
		}},
		{name: "two-exact-blocks", build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocByteBackfillSizedFixture(test, 2*ChunkTarget)
		}},
		{name: "large-event", build: legacyDocByteGenerationLargeFixture},
		{name: "split-utf8", build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocByteBackfillBoundaryFixture(test, "\ud55c🙂")
		}},
		{name: "split-escape", build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocByteBackfillBoundaryFixture(test, "\"\\\n\t\x01<>&\u2028\u2029")
		}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			proof, original := fixture.build(test)
			originalRef := proof.DocumentRef()
			originalChunks, originalChunked := proof.ChunkPlan()
			envelope, stream := chunkVerifierParts(test, original)
			plan, spec, key := legacyDocByteBackfillRequirePlan(test, proof, stream)
			var joined []byte
			for ordinal := 0; ordinal < spec.Blocks; ordinal++ {
				block, err := plan.Block(context.Background(), ordinal)
				if err != nil {
					test.Fatalf("read block %d: %v", ordinal, err)
				}
				legacyDocByteBackfillRequireBlock(test, block, spec, key, ordinal, stream)
				joined = append(joined, block.Bytes()...)
			}
			if !bytes.Equal(joined, stream) || !bytes.Equal(AssembleCanonicalDoc(envelope, [][]byte{joined}), original) {
				test.Fatal("backfill added envelope or wrapper bytes, or changed the events-array interior")
			}
			for ordinal := spec.Blocks - 1; ordinal >= 0; ordinal-- {
				for _, selected := range []int{ordinal, 0, spec.Blocks - 1, ordinal} {
					block, err := plan.Block(context.Background(), selected)
					if err != nil {
						test.Fatalf("seek block %d: %v", selected, err)
					}
					legacyDocByteBackfillRequireBlock(test, block, spec, key, selected, stream)
				}
			}
			repeated, repeatedSpec, repeatedKey := legacyDocByteBackfillRequirePlan(test, proof, stream)
			if !repeated.Valid() || repeatedSpec != spec || repeatedKey != key {
				test.Fatal("repeated construction changed the backfill spec or key")
			}
			chunks, chunked := proof.ChunkPlan()
			if proof.DocumentRef() != originalRef || !bytes.Equal(proof.Bytes(), original) ||
				chunked != originalChunked || !reflect.DeepEqual(chunks, originalChunks) {
				test.Fatal("backfill changed the original verified document or its chunk representation")
			}
			if _, exists := proof.ConversationManifest(); exists {
				test.Fatal("backfill upgraded the legacy proof to a root identity")
			}
		})
	}
}

func TestLegacyByteBackfillRepresentationParity(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		build func(testing.TB) (VerifiedSessionDoc, []byte)
	}{
		{name: "multiple-events", build: func(test testing.TB) (VerifiedSessionDoc, []byte) { return legacyDocRangeGenerationFixture(test, 9) }},
		{name: "large-event", build: legacyDocByteGenerationLargeFixture},
		{name: "split-utf8", build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocByteBackfillBoundaryFixture(test, "\ud55c🙂")
		}},
		{name: "split-escape", build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocByteBackfillBoundaryFixture(test, "\"\\\n\t")
		}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			materialized, raw := fixture.build(test)
			envelope, stream := chunkVerifierParts(test, raw)
			_, wantSpec, wantKey := legacyDocByteBackfillRequirePlan(test, materialized, stream)
			var verifier CanonicalDocVerifier
			canonical, err := verifier.Verify(context.Background(), materialized.Hash(), raw)
			if err != nil {
				test.Fatal(err)
			}
			proofs := map[string]VerifiedSessionDoc{"canonical-monolith": canonical}
			v1, ok := PlanDocChunksV1(raw)
			if !ok {
				test.Fatal("fixture cannot use the legacy event-boundary representation")
			}
			inputs := map[string]DocChunkPlan{"v1": v1}
			unversioned := v1
			unversioned.Manifest.Format = ""
			inputs["unversioned-v1"] = unversioned
			for _, width := range []int{1021, ChunkTarget - 1, ChunkTarget, ChunkTarget + 1} {
				inputs[fmt.Sprintf("v2-width-%d", width)] = chunkVerifierPlan(envelope, stream, width)
			}
			for name, input := range inputs {
				proof, err := verifier.VerifyChunks(context.Background(), materialized.Hash(), input.Manifest, chunkVerifierLoad(input))
				if err != nil {
					test.Fatalf("verify %s: %v", name, err)
				}
				proofs[name] = proof
			}
			for name, proof := range proofs {
				test.Run(name, func(test *testing.T) {
					plan, spec, key := legacyDocByteBackfillRequirePlan(test, proof, stream)
					if spec != wantSpec || key != wantKey {
						test.Fatal("source representation changed the normalized backfill spec or key")
					}
					for ordinal := spec.Blocks - 1; ordinal >= 0; ordinal-- {
						block, err := plan.Block(context.Background(), ordinal)
						if err != nil {
							test.Fatal(err)
						}
						legacyDocByteBackfillRequireBlock(test, block, wantSpec, wantKey, ordinal, stream)
					}
					if proof.DocumentRef() != materialized.DocumentRef() || !bytes.Equal(proof.Bytes(), raw) {
						test.Fatal("backfill changed the original legacy representation's identity or canonical bytes")
					}
				})
			}
		})
	}
}

func TestLegacyByteBackfillSpecRejectsInvalidMetadata(test *testing.T) {
	proof, raw := legacyDocByteGenerationLargeFixture(test)
	_, stream := chunkVerifierParts(test, raw)
	_, spec, _ := legacyDocByteBackfillRequirePlan(test, proof, stream)
	for name, mutate := range map[string]func(*DocLegacyByteBackfillSpec){
		"zero":             func(value *DocLegacyByteBackfillSpec) { *value = DocLegacyByteBackfillSpec{} },
		"zero-version":     func(value *DocLegacyByteBackfillSpec) { value.Version = 0 },
		"negative-version": func(value *DocLegacyByteBackfillSpec) { value.Version = -1 },
		"future-version":   func(value *DocLegacyByteBackfillSpec) { value.Version = 2 },
		"missing-hash":     func(value *DocLegacyByteBackfillSpec) { value.Reference.Hash = "" },
		"invalid-hash":     func(value *DocLegacyByteBackfillSpec) { value.Reference.Hash = "sha256:not-a-digest" },
		"root-identity":    func(value *DocLegacyByteBackfillSpec) { value.Reference.Identity = DocumentIdentityRootV1 },
		"future-identity":  func(value *DocLegacyByteBackfillSpec) { value.Reference.Identity = "future" },
		"negative-stream":  func(value *DocLegacyByteBackfillSpec) { value.StreamBytes = -1 },
		"oversized-stream": func(value *DocLegacyByteBackfillSpec) {
			value.StreamBytes = MaxFinalizedDocBytes + 1
			value.Blocks = int((value.StreamBytes + ChunkTarget - 1) / ChunkTarget)
		},
		"overflow-stream": func(value *DocLegacyByteBackfillSpec) { value.StreamBytes = 1<<63 - 1 },
		"zero-width":      func(value *DocLegacyByteBackfillSpec) { value.BlockBytes = 0 },
		"negative-width":  func(value *DocLegacyByteBackfillSpec) { value.BlockBytes = -1 },
		"short-width":     func(value *DocLegacyByteBackfillSpec) { value.BlockBytes-- },
		"long-width":      func(value *DocLegacyByteBackfillSpec) { value.BlockBytes++ },
		"negative-blocks": func(value *DocLegacyByteBackfillSpec) { value.Blocks = -1 },
		"missing-blocks":  func(value *DocLegacyByteBackfillSpec) { value.Blocks = 0 },
		"too-few-blocks":  func(value *DocLegacyByteBackfillSpec) { value.Blocks-- },
		"too-many-blocks": func(value *DocLegacyByteBackfillSpec) { value.Blocks++ },
		"overflow-blocks": func(value *DocLegacyByteBackfillSpec) { value.Blocks = int(^uint(0) >> 1) },
		"empty-with-blocks": func(value *DocLegacyByteBackfillSpec) {
			value.StreamBytes, value.Blocks = 0, 1
		},
	} {
		test.Run(name, func(test *testing.T) {
			invalid := spec
			mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				test.Fatal("invalid backfill spec validated")
			}
			if encoded, err := invalid.Encode(); err == nil || encoded != nil {
				test.Fatalf("invalid backfill spec encoded: %s, %v", encoded, err)
			}
			encoded, err := json.Marshal(invalid)
			if err != nil {
				return
			}
			decoded, err := DecodeDocLegacyByteBackfillSpec(HashContent(encoded), encoded)
			if err == nil || decoded != (DocLegacyByteBackfillSpec{}) {
				test.Fatalf("invalid authenticated backfill spec decoded: %+v, %v", decoded, err)
			}
		})
	}
}

func TestLegacyByteBackfillSpecRejectsCorruptAndNoncanonicalWire(test *testing.T) {
	proof, raw := legacyDocRangeGenerationFixture(test, 9)
	_, stream := chunkVerifierParts(test, raw)
	_, spec, key := legacyDocByteBackfillRequirePlan(test, proof, stream)
	encoded, err := spec.Encode()
	if err != nil {
		test.Fatal(err)
	}
	pretty, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		test.Fatal(err)
	}
	corrupt := bytes.Clone(encoded)
	corrupt[0] ^= 1
	candidates := map[string][]byte{
		"missing": nil, "null": []byte(`null`), "array": []byte(`[]`), "string": []byte(`"spec"`),
		"malformed": []byte(`{`), "truncated": encoded[:len(encoded)-1], "corrupt": corrupt,
		"leading-space": append([]byte{' '}, encoded...), "trailing-space": append(bytes.Clone(encoded), '\n'),
		"pretty": pretty, "trailing-value": append(bytes.Clone(encoded), []byte(`{}`)...),
		"unknown-field": append(bytes.Clone(encoded[:len(encoded)-1]), []byte(`,"unexpected":true}`)...),
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		test.Fatal(err)
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		field := fields[name]
		delete(fields, name)
		missing, err := canonicalJSON(fields)
		if err != nil {
			test.Fatal(err)
		}
		candidates["missing-"+name] = missing
		fields[name] = field
		member, err := json.Marshal(name)
		if err != nil {
			test.Fatal(err)
		}
		duplicate := append(bytes.Clone(encoded[:len(encoded)-1]), ',')
		duplicate = append(duplicate, member...)
		duplicate = append(duplicate, ':')
		duplicate = append(duplicate, field...)
		candidates["duplicate-"+name] = append(duplicate, '}')
	}
	for name, candidate := range candidates {
		test.Run(name, func(test *testing.T) {
			for _, expected := range []ContentHash{key, HashContent(candidate)} {
				decoded, err := DecodeDocLegacyByteBackfillSpec(expected, candidate)
				if err == nil || decoded != (DocLegacyByteBackfillSpec{}) {
					test.Fatalf("corrupt or noncanonical spec returned metadata: %+v, %v", decoded, err)
				}
			}
		})
	}
	for _, expected := range []ContentHash{"", "invalid", HashContent([]byte("another backfill plan"))} {
		if decoded, err := DecodeDocLegacyByteBackfillSpec(expected, encoded); err == nil || decoded != (DocLegacyByteBackfillSpec{}) {
			test.Fatalf("spec accepted wrong expected key %q: %+v, %v", expected, decoded, err)
		}
	}
}

func TestLegacyByteBackfillRejectsRootAndInvalidProofs(test *testing.T) {
	legacy, raw := legacyDocRangeGenerationFixture(test, 1)
	var document CIRDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		test.Fatal(err)
	}
	manifest, bodies, err := ConversationManifestForCIR(document)
	if err != nil {
		test.Fatal(err)
	}
	var verifier CanonicalDocVerifier
	root, err := verifier.VerifyConversationManifestDoc(context.Background(), manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies))
	if err != nil || !root.Valid() || root.DocumentRef().Identity != DocumentIdentityRootV1 {
		test.Fatalf("root fixture: %v", err)
	}
	for name, proof := range map[string]VerifiedSessionDoc{
		"root": root, "zero": {}, "hash-only": {hash: legacy.Hash()}, "canonical-only": {canonical: string(raw)},
		"unknown-identity": {hash: legacy.Hash(), canonical: string(raw), identity: "future"},
		"broken-wrapper":   {hash: legacy.Hash(), canonical: `{}`},
	} {
		test.Run(name, func(test *testing.T) {
			plan, err := NewLegacyByteBackfill(context.Background(), proof)
			if err == nil || plan.Valid() {
				test.Fatalf("unsupported or invalid proof produced a valid backfill plan: %v", err)
			}
		})
	}
	if !bytes.Equal(root.Bytes(), raw) || !bytes.Equal(legacy.Bytes(), raw) {
		test.Fatal("backfill rejection changed a source proof")
	}
}

func TestLegacyByteBackfillOwnsReturnedValuesAndSourceBytes(test *testing.T) {
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{"original-model"}}, Events: []CIREvent{
		{Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "immutable-message-\ud55c🙂\"\\\n"}}},
	}}
	materialized, original := legacyDocByteBackfillDocumentFixture(test, document)
	envelope, stream := chunkVerifierParts(test, original)
	input := chunkVerifierPlan(envelope, stream, 7)
	var verifier CanonicalDocVerifier
	chunked, err := verifier.VerifyChunks(context.Background(), materialized.Hash(), input.Manifest, chunkVerifierLoad(input))
	if err != nil {
		test.Fatal(err)
	}
	for _, proof := range []VerifiedSessionDoc{materialized, chunked} {
		plan, baseline, key := legacyDocByteBackfillRequirePlan(test, proof, stream)
		block, err := plan.Block(context.Background(), 0)
		if err != nil {
			test.Fatal(err)
		}
		first, second := block.Bytes(), block.Bytes()
		first[0] ^= 1
		if !bytes.Equal(second, stream) {
			test.Fatal("separate block byte results alias each other")
		}
		second[len(second)-1] ^= 1
		changedSpec, err := plan.Spec()
		if err != nil {
			test.Fatal(err)
		}
		changedSpec.Reference.Hash = HashContent([]byte("caller reference"))
		changedSpec.Reference.Identity = DocumentIdentityRootV1
		changedSpec.BlockBytes, changedSpec.Blocks, changedSpec.StreamBytes = 1, 0, 0
		descriptor := block.Descriptor()
		descriptor.Hash, descriptor.Bytes = "invalid", -1
		reference := block.Reference()
		reference.Hash, reference.Identity = "invalid", DocumentIdentityRootV1
		encoded, err := baseline.Encode()
		if err != nil {
			test.Fatal(err)
		}
		decoded, err := DecodeDocLegacyByteBackfillSpec(key, encoded)
		if err != nil {
			test.Fatal(err)
		}
		encoded[0] ^= 1
		reencoded, err := decoded.Encode()
		if err != nil || HashContent(reencoded) != key {
			test.Fatalf("decoded spec retained caller-owned wire bytes: %v", err)
		}
		mutableProof := proof.Bytes()
		mutableProof[0] ^= 1
		exported, ok := proof.ChunkPlan()
		if !ok {
			test.Fatal("nonempty source has no chunk plan")
		}
		exported.Manifest.Envelope[0] ^= 1
		exported.Manifest.Chunks[0] = "invalid"
		for _, body := range exported.Bodies {
			body[0] ^= 1
		}
		progress, err := plan.CopyBatch(context.Background(), 0, 1, func(ctx context.Context, saved VerifiedLegacyByteBlock) error {
			legacyDocByteBackfillRequireBlock(test, saved, baseline, key, 0, stream)
			mutable := saved.Bytes()
			mutable[0] ^= 1
			legacyDocByteBackfillRequireBlock(test, saved, baseline, key, 0, stream)
			return ctx.Err()
		})
		if err != nil || progress != (DocLegacyByteBackfillProgress{Plan: key, Next: 1, CopiedBytes: int64(len(stream)), Complete: true}) {
			test.Fatalf("callback mutation changed acknowledged progress: %+v, %v", progress, err)
		}
		legacyDocByteBackfillRequireBlock(test, block, baseline, key, 0, stream)
		_, repeatedSpec, repeatedKey := legacyDocByteBackfillRequirePlan(test, proof, stream)
		if repeatedSpec != baseline || repeatedKey != key || !bytes.Equal(proof.Bytes(), original) {
			test.Fatal("caller mutation changed the original proof or backfill metadata")
		}
	}
	document.Envelope.SourceModels[0] = "changed-model"
	document.Events[0].Blocks[0].Text = "changed-message"
	input.Manifest.Envelope[0] ^= 1
	input.Manifest.Chunks[0] = "invalid"
	for _, body := range input.Bodies {
		body[0] ^= 1
	}
	for _, proof := range []VerifiedSessionDoc{materialized, chunked} {
		plan, spec, key := legacyDocByteBackfillRequirePlan(test, proof, stream)
		block, err := plan.Block(context.Background(), 0)
		if err != nil || !bytes.Equal(proof.Bytes(), original) {
			test.Fatalf("caller-owned source mutation changed a verified proof: %v", err)
		}
		legacyDocByteBackfillRequireBlock(test, block, spec, key, 0, stream)
	}
}

func TestLegacyByteBackfillBatchCapProgressAndIdempotentResume(test *testing.T) {
	if DocLegacyByteBackfillBatchBlocks != 8 {
		test.Fatalf("backfill batch cap is %d, want 8", DocLegacyByteBackfillBatchBlocks)
	}
	proof, raw := legacyDocByteBackfillSizedFixture(test, 10*ChunkTarget+17)
	_, stream := chunkVerifierParts(test, raw)
	plan, spec, key := legacyDocByteBackfillRequirePlan(test, proof, stream)
	stored := make(map[int]VerifiedLegacyByteBlock)
	var ordinals []int
	save := func(ctx context.Context, block VerifiedLegacyByteBlock) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		legacyDocByteBackfillRequireBlock(test, block, spec, key, block.Ordinal(), stream)
		if previous, exists := stored[block.Ordinal()]; exists &&
			(previous.Descriptor() != block.Descriptor() || !bytes.Equal(previous.Bytes(), block.Bytes())) {
			test.Fatal("replaying an acknowledged ordinal changed its bytes or descriptor")
		}
		stored[block.Ordinal()] = block
		ordinals = append(ordinals, block.Ordinal())
		return nil
	}
	for _, batch := range []struct {
		name      string
		start     int
		limit     int
		next      int
		copied    int64
		completed bool
	}{
		{name: "eight-block-cap", start: 0, limit: 8, next: 8, copied: 8 * ChunkTarget},
		{name: "idempotent-replay", start: 0, limit: 8, next: 8, copied: 8 * ChunkTarget},
		{name: "resume-with-tail", start: 8, limit: 8, next: 11, copied: 2*ChunkTarget + 17, completed: true},
		{name: "seek-middle", start: 3, limit: 2, next: 5, copied: 2 * ChunkTarget},
		{name: "single-tail", start: 10, limit: 1, next: 11, copied: 17, completed: true},
	} {
		test.Run(batch.name, func(test *testing.T) {
			ordinals = nil
			progress, err := plan.CopyBatch(context.Background(), batch.start, batch.limit, save)
			want := DocLegacyByteBackfillProgress{Plan: key, Next: batch.next, CopiedBytes: batch.copied, Complete: batch.completed}
			if err != nil || progress != want || len(ordinals) != batch.next-batch.start {
				test.Fatalf("batch progress %+v, %v, callbacks %v; want %+v", progress, err, ordinals, want)
			}
			for offset, ordinal := range ordinals {
				if ordinal != batch.start+offset {
					test.Fatalf("batch skipped or reordered an ordinal: %v", ordinals)
				}
			}
		})
	}
	for limit := 1; limit <= 8; limit++ {
		ordinals = nil
		progress, err := plan.CopyBatch(context.Background(), 1, limit, save)
		want := DocLegacyByteBackfillProgress{Plan: key, Next: 1 + limit, CopiedBytes: int64(limit) * ChunkTarget}
		if err != nil || progress != want || len(ordinals) != limit {
			test.Fatalf("valid budget %d: progress %+v, %v, callbacks %v; want %+v", limit, progress, err, ordinals, want)
		}
		for offset, ordinal := range ordinals {
			if ordinal != 1+offset {
				test.Fatalf("valid budget %d reordered callbacks: %v", limit, ordinals)
			}
		}
	}
	progress, err := plan.CopyBatch(context.Background(), spec.Blocks, 8, nil)
	if err != nil || progress != (DocLegacyByteBackfillProgress{Plan: key, Next: spec.Blocks, Complete: true}) || len(stored) != spec.Blocks {
		test.Fatalf("terminal nil-save batch: %+v, %v, stored=%d", progress, err, len(stored))
	}
	if !bytes.Equal(proof.Bytes(), raw) {
		test.Fatal("batch copying rewrote the original document")
	}
}

func TestLegacyByteBackfillCallbackFailurePreservesAcknowledgedProgress(test *testing.T) {
	proof, raw := legacyDocByteGenerationLargeFixture(test)
	_, stream := chunkVerifierParts(test, raw)
	plan, spec, key := legacyDocByteBackfillRequirePlan(test, proof, stream)
	failure := errors.New("backfill save failed")
	for _, start := range []int{0, 1} {
		for failAt := start; failAt < spec.Blocks; failAt++ {
			test.Run(fmt.Sprintf("start-%d/fail-%d", start, failAt), func(test *testing.T) {
				calls := 0
				progress, err := plan.CopyBatch(context.Background(), start, 8, func(ctx context.Context, block VerifiedLegacyByteBlock) error {
					ordinal := start + calls
					calls++
					legacyDocByteBackfillRequireBlock(test, block, spec, key, ordinal, stream)
					if ordinal == failAt {
						return failure
					}
					return ctx.Err()
				})
				want := DocLegacyByteBackfillProgress{Plan: key, Next: failAt, CopiedBytes: int64(failAt-start) * ChunkTarget}
				if !errors.Is(err, failure) || progress != want || calls != failAt-start+1 {
					test.Fatalf("failed callback advanced or lost acknowledged progress: %+v, %v, calls=%d; want %+v", progress, err, calls, want)
				}
				next := failAt
				resumed, err := plan.CopyBatch(context.Background(), progress.Next, 8, func(ctx context.Context, block VerifiedLegacyByteBlock) error {
					legacyDocByteBackfillRequireBlock(test, block, spec, key, next, stream)
					next++
					return ctx.Err()
				})
				want = DocLegacyByteBackfillProgress{Plan: key, Next: spec.Blocks, CopiedBytes: spec.StreamBytes - int64(failAt)*ChunkTarget, Complete: true}
				if err != nil || resumed != want || next != spec.Blocks {
					test.Fatalf("resume did not retry the unacknowledged ordinal: %+v, %v; want %+v", resumed, err, want)
				}
			})
		}
	}
}

func TestLegacyByteBackfillZeroValuesAndInvalidArguments(test *testing.T) {
	var zero LegacyByteBackfill
	var block VerifiedLegacyByteBlock
	if zero.Valid() || block.Valid() || block.Reference() != (DocumentRef{}) || block.PlanKey() != "" ||
		block.Descriptor() != (ConversationManifestChunk{}) || len(block.Bytes()) != 0 {
		test.Fatal("zero backfill plan or block claims verified content")
	}
	if spec, err := zero.Spec(); err == nil || spec != (DocLegacyByteBackfillSpec{}) {
		test.Fatalf("zero plan returned a spec: %+v, %v", spec, err)
	}
	if key, err := zero.Key(); err == nil || key != "" {
		test.Fatalf("zero plan returned a key: %s, %v", key, err)
	}
	if got, err := zero.Block(context.Background(), 0); err == nil || got.Valid() || len(got.Bytes()) != 0 {
		test.Fatalf("zero plan returned a block: %v", err)
	}
	calls := 0
	save := func(context.Context, VerifiedLegacyByteBlock) error { calls++; return nil }
	if progress, err := zero.CopyBatch(context.Background(), 0, 1, save); err == nil || progress != (DocLegacyByteBackfillProgress{}) || calls != 0 {
		test.Fatalf("zero plan copied blocks: %+v, %v", progress, err)
	}
	proof, raw := legacyDocByteGenerationLargeFixture(test)
	_, stream := chunkVerifierParts(test, raw)
	plan, spec, _ := legacyDocByteBackfillRequirePlan(test, proof, stream)
	maximum := int(^uint(0) >> 1)
	for _, ordinal := range []int{-1, spec.Blocks, spec.Blocks + 1, maximum} {
		if got, err := plan.Block(context.Background(), ordinal); err == nil || got.Valid() || len(got.Bytes()) != 0 {
			test.Fatalf("invalid ordinal %d returned block bytes: %v", ordinal, err)
		}
	}
	for _, invalid := range []struct {
		name    string
		start   int
		limit   int
		nilSave bool
	}{
		{name: "negative-start", start: -1, limit: 1},
		{name: "past-end", start: spec.Blocks + 1, limit: 1},
		{name: "overflow-start", start: maximum, limit: 1},
		{name: "zero-budget", limit: 0},
		{name: "negative-budget", limit: -1},
		{name: "over-eight-block-cap", limit: 9},
		{name: "overflow-budget", limit: maximum},
		{name: "nil-save", limit: 1, nilSave: true},
		{name: "resumed-nil-save", start: 1, limit: 1, nilSave: true},
		{name: "terminal-zero-budget", start: spec.Blocks, nilSave: true},
		{name: "terminal-over-cap", start: spec.Blocks, limit: 9, nilSave: true},
	} {
		test.Run(invalid.name, func(test *testing.T) {
			callback := save
			if invalid.nilSave {
				callback = nil
			}
			progress, err := plan.CopyBatch(context.Background(), invalid.start, invalid.limit, callback)
			if !errors.Is(err, ErrValidation) || progress != (DocLegacyByteBackfillProgress{}) || calls != 0 {
				test.Fatalf("invalid batch arguments invoked save or returned progress: %+v, %v, calls=%d", progress, err, calls)
			}
		})
	}
}

func TestLegacyByteBackfillEmptyStream(test *testing.T) {
	proof, raw := legacyDocRangeGenerationFixture(test, 0)
	_, stream := chunkVerifierParts(test, raw)
	plan, spec, key := legacyDocByteBackfillRequirePlan(test, proof, stream)
	if spec.Blocks != 0 || spec.StreamBytes != 0 {
		test.Fatal("empty document acquired a phantom byte block")
	}
	calls := 0
	for _, save := range []func(context.Context, VerifiedLegacyByteBlock) error{
		nil, func(context.Context, VerifiedLegacyByteBlock) error { calls++; return errors.New("unexpected save") },
	} {
		progress, err := plan.CopyBatch(context.Background(), 0, 1, save)
		if err != nil || progress != (DocLegacyByteBackfillProgress{Plan: key, Complete: true}) || calls != 0 {
			test.Fatalf("empty backfill invoked save or returned wrong progress: %+v, %v", progress, err)
		}
	}
	if block, err := plan.Block(context.Background(), 0); err == nil || block.Valid() || len(block.Bytes()) != 0 {
		test.Fatalf("empty backfill returned envelope, wrapper, or empty-block bytes: %v", err)
	}
	if !bytes.Equal(proof.Bytes(), raw) {
		test.Fatal("empty backfill rewrote the original proof")
	}
}

func TestLegacyByteBackfillCancellationPreservesAcknowledgedProgress(test *testing.T) {
	proof, raw := legacyDocByteGenerationLargeFixture(test)
	_, stream := chunkVerifierParts(test, raw)
	plan, spec, key := legacyDocByteBackfillRequirePlan(test, proof, stream)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := NewLegacyByteBackfill(canceled, proof); !errors.Is(err, context.Canceled) || got.Valid() {
		test.Fatalf("canceled constructor returned a backfill plan: %v", err)
	}
	if got, err := plan.Block(canceled, 0); !errors.Is(err, context.Canceled) || got.Valid() || len(got.Bytes()) != 0 {
		test.Fatalf("canceled block read returned verified bytes: %v", err)
	}
	calls := 0
	for _, start := range []int{0, 1, spec.Blocks} {
		var save func(context.Context, VerifiedLegacyByteBlock) error
		if start < spec.Blocks {
			save = func(context.Context, VerifiedLegacyByteBlock) error { calls++; return nil }
		}
		progress, err := plan.CopyBatch(canceled, start, 1, save)
		if !errors.Is(err, context.Canceled) || progress != (DocLegacyByteBackfillProgress{}) || calls != 0 {
			test.Fatalf("canceled entry returned progress or invoked save: %+v, %v", progress, err)
		}
	}
	for _, acknowledgeCanceled := range []bool{false, true} {
		test.Run(fmt.Sprintf("acknowledge-canceled-%t", acknowledgeCanceled), func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			progress, err := plan.CopyBatch(ctx, 0, 8, func(saveContext context.Context, block VerifiedLegacyByteBlock) error {
				ordinal := calls
				calls++
				legacyDocByteBackfillRequireBlock(test, block, spec, key, ordinal, stream)
				if ordinal == 1 {
					cancel()
					if !acknowledgeCanceled {
						return saveContext.Err()
					}
				}
				return nil
			})
			next := 1
			if acknowledgeCanceled {
				next++
			}
			want := DocLegacyByteBackfillProgress{Plan: key, Next: next, CopiedBytes: int64(next) * ChunkTarget}
			if !errors.Is(err, context.Canceled) || progress != want || calls != 2 {
				test.Fatalf("cancellation discarded an acknowledgment or acknowledged a failed save: %+v, %v, calls=%d; want %+v", progress, err, calls, want)
			}
			resumed, err := plan.CopyBatch(context.Background(), progress.Next, 8, func(ctx context.Context, block VerifiedLegacyByteBlock) error {
				legacyDocByteBackfillRequireBlock(test, block, spec, key, next, stream)
				next++
				return ctx.Err()
			})
			want = DocLegacyByteBackfillProgress{Plan: key, Next: spec.Blocks, CopiedBytes: spec.StreamBytes - int64(progress.Next)*ChunkTarget, Complete: true}
			if err != nil || resumed != want || next != spec.Blocks {
				test.Fatalf("resume after cancellation changed the acknowledged boundary: %+v, %v; want %+v", resumed, err, want)
			}
		})
	}
	test.Run("cancel-before-next-save", func(test *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		checked := canonicalAdmissionContext{Context: ctx, check: func() {
			if calls == 1 {
				cancel()
			}
		}}
		progress, err := plan.CopyBatch(checked, 1, 8, func(ctx context.Context, block VerifiedLegacyByteBlock) error {
			legacyDocByteBackfillRequireBlock(test, block, spec, key, 1+calls, stream)
			calls++
			return nil
		})
		want := DocLegacyByteBackfillProgress{Plan: key, Next: 2, CopiedBytes: ChunkTarget}
		if !errors.Is(err, context.Canceled) || progress != want || calls != 1 {
			test.Fatalf("cancellation between saves discarded a prior acknowledgment: %+v, %v, calls=%d; want %+v", progress, err, calls, want)
		}
	})
	if !bytes.Equal(proof.Bytes(), raw) {
		test.Fatal("cancellation changed the original proof")
	}
}
