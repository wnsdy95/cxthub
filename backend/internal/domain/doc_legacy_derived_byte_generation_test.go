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
	"unicode/utf8"
)

func legacyDerivedDocByteGenerationChunks(test testing.TB, generation DocRangeGeneration, objects []DocRangeObject, plan LegacyByteBackfill, stream []byte) []ConversationManifestChunk {
	test.Helper()
	bodies := docRangeGenerationObjects(test, objects)
	visited := make(map[ContentHash]bool, len(bodies))
	loadBody := docRangeGenerationLoad(bodies)
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		raw, err := loadBody(ctx, hash)
		if err == nil {
			visited[hash] = true
		}
		return raw, err
	}
	chunks := make([]ConversationManifestChunk, 0, generation.ChunkCount())
	for position := 0; position < generation.ChunkPages.Count; position++ {
		selected, err := ResolveDocRangeLeaf(context.Background(), generation.ChunkPages, position, load)
		if err != nil {
			test.Fatalf("resolve derived chunk page %d: %v", position, err)
		}
		raw, err := load(context.Background(), selected)
		if err != nil {
			test.Fatal(err)
		}
		page, err := generation.DecodeChunks(position, selected, raw)
		if err != nil || !bytes.Equal(raw, docRangeGenerationJSON(test, page)) {
			test.Fatalf("derived chunk page %d is not canonical descriptor metadata: %v", position, err)
		}
		chunks = append(chunks, page...)
	}
	spec, err := plan.Spec()
	if err != nil || len(chunks) != spec.Blocks || len(chunks) != generation.ChunkCount() {
		test.Fatalf("derived chunk count differs from backfill: chunks=%d spec=%+v, %v", len(chunks), spec, err)
	}
	var joined []byte
	for ordinal, chunk := range chunks {
		block, err := plan.Block(context.Background(), ordinal)
		if err != nil {
			test.Fatalf("read derived backfill block %d: %v", ordinal, err)
		}
		start := ordinal * (512 << 10)
		want := stream[start:min(start+(512<<10), len(stream))]
		body := block.Bytes()
		if !block.Valid() || block.Ordinal() != ordinal || block.Reference() != spec.Reference || chunk != block.Descriptor() ||
			chunk.Hash != HashContent(want) || chunk.Hash != HashContent(body) || chunk.Bytes != int64(len(want)) ||
			chunk.Bytes != int64(len(body)) || !bytes.Equal(body, want) {
			test.Fatalf("derived chunk %d changed backfill ordinal, hash, length, or exact stream bytes", ordinal)
		}
		if _, exists := bodies[chunk.Hash]; exists {
			test.Fatalf("derived byte payload %d was returned as a metadata object", ordinal)
		}
		joined = append(joined, body...)
	}
	if !bytes.Equal(joined, stream) || int64(len(joined)) != generation.StreamBytes || len(visited) != len(bodies) {
		test.Fatalf("derived metadata changed stream extent or emitted unreachable objects: bytes=%d visited=%d objects=%d", len(joined), len(visited), len(bodies))
	}
	return chunks
}

func TestLegacyDerivedDocByteRangeGenerationMetadataAndBackfillParity(test *testing.T) {
	for _, fixture := range []struct {
		name       string
		build      func(testing.TB) (VerifiedSessionDoc, []byte)
		wantChunks int
	}{
		{name: "empty", wantChunks: 0, build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocRangeGenerationFixture(test, 0)
		}},
		{name: "mixed-events", wantChunks: 1, build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocRangeGenerationFixture(test, 9)
		}},
		{name: "event-page-boundary", wantChunks: 1, build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocRangeGenerationFixture(test, 2*DocRangePageEvents+7)
		}},
		{name: "exact-chunk-boundary", wantChunks: 1, build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocByteBackfillSizedFixture(test, 512<<10)
		}},
		{name: "three-chunks", build: legacyDocByteGenerationLargeFixture, wantChunks: 3},
		{name: "split-utf8", wantChunks: 2, build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocByteBackfillBoundaryFixture(test, "\ud55c🙂")
		}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			proof, original := fixture.build(test)
			originalRef := proof.DocumentRef()
			originalPlan, originalChunked := proof.ChunkPlan()
			envelope, stream := chunkVerifierParts(test, original)
			baseline, baselineObjects, err := BuildLegacyDocRangeGeneration(context.Background(), proof)
			if err != nil {
				test.Fatal(err)
			}
			baselineBytes, err := baseline.Encode()
			if err != nil {
				test.Fatal(err)
			}
			generation, objects, err := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
			if err != nil {
				test.Fatal(err)
			}
			if generation.Version != 5 || generation.SourceIdentity == nil || *generation.SourceIdentity != DocumentIdentityLegacy ||
				generation.Identity() != DocumentIdentityLegacy || generation.ByteStorage() != DocRangeByteStorageDerived ||
				generation.Hash != HashContent(original) || generation.Algorithm != 1 || !bytes.Equal(generation.Envelope, envelope) ||
				generation.StreamBytes != int64(len(stream)) || generation.ChunkCount() != fixture.wantChunks ||
				generation.ChunkPages.Count != (fixture.wantChunks+DocRangeChunkPageSize-1)/DocRangeChunkPageSize {
				test.Fatalf("derived generation changed legacy identity, envelope, or normalized partition: %+v", generation)
			}
			metadataOnly := generation
			metadataOnly.Version, metadataOnly.ChunkPages = 3, DocRangeDirectoryRoot{}
			metadataBytes, err := metadataOnly.Encode()
			if err != nil || baseline.Version != 3 || baseline.ByteStorage() != DocRangeByteStorageNone ||
				!reflect.DeepEqual(metadataOnly, baseline) || !bytes.Equal(metadataBytes, baselineBytes) ||
				len(objects) < len(baselineObjects) || !reflect.DeepEqual(objects[:len(baselineObjects)], baselineObjects) {
				test.Fatalf("stripping v5 byte enrichment did not preserve exact v3 metadata and object order: %v", err)
			}
			entries := legacyDocRangeGenerationPages(test, metadataOnly, objects[:len(baselineObjects)])
			var document struct {
				Events []json.RawMessage `json:"events"`
			}
			if err := json.Unmarshal(original, &document); err != nil {
				test.Fatal(err)
			}
			offset, rendered := 0, 0
			for position, entry := range entries {
				var event CIREvent
				if err := json.Unmarshal(document.Events[position], &event); err != nil {
					test.Fatal(err)
				}
				empty := event.Kind == EventReasoning && strings.Trim(event.RedactedSummary, " \ufeff\t\n") == ""
				if entry.Index != position || entry.Offset != offset || entry.Length != len(document.Events[position]) ||
					entry.Hash != HashContent(document.Events[position]) || entry.Seq != event.Seq || entry.Kind != event.Kind ||
					entry.RenderedBefore != rendered || entry.EmptyReasoning != empty {
					test.Fatalf("original event %d changed order, location, hash, sequence, or rendered counters: %+v", position, entry)
				}
				offset += entry.Length + 1
				if !empty {
					rendered++
				}
			}
			encoded, err := generation.Encode()
			if err != nil || len(encoded) > MaxDocRangeGenerationBytes || !bytes.Contains(encoded, []byte(`"source_identity":""`)) ||
				!bytes.Equal(encoded, docRangeGenerationJSON(test, generation)) {
				test.Fatalf("v5 descriptor is not canonical bounded legacy metadata: %v", err)
			}
			decoded, err := DecodeDocRangeGeneration(proof.Hash(), HashContent(encoded), encoded)
			if err != nil || !reflect.DeepEqual(decoded, generation) {
				test.Fatalf("v5 descriptor round trip: %v", err)
			}
			reencoded, err := decoded.Encode()
			if err != nil || !bytes.Equal(reencoded, encoded) {
				test.Fatalf("v5 descriptor re-encoding changed canonical bytes: %v", err)
			}
			plan, err := NewLegacyByteBackfill(context.Background(), proof)
			if err != nil {
				test.Fatal(err)
			}
			spec, err := plan.Spec()
			if err != nil || spec.Reference != originalRef || spec.StreamBytes != generation.StreamBytes || spec.BlockBytes != 512<<10 {
				test.Fatalf("derived generation disagrees with the normalized legacy backfill spec: %+v, %v", spec, err)
			}
			chunks := legacyDerivedDocByteGenerationChunks(test, decoded, objects[len(baselineObjects):], plan, stream)
			for _, chunk := range chunks {
				for _, object := range objects {
					if object.Hash == chunk.Hash {
						test.Fatal("derived payload was copied into event or chunk metadata objects")
					}
				}
			}
			if fixture.name == "split-utf8" && (!utf8.Valid(stream) || utf8.Valid(stream[:512<<10]) || utf8.Valid(stream[512<<10:])) {
				test.Fatal("fixture did not preserve a UTF-8 code point split across normalized byte chunks")
			}
			if fixture.wantChunks == 0 && (len(objects) != 0 || generation.EventPages != (DocRangeDirectoryRoot{}) || generation.ChunkPages != (DocRangeDirectoryRoot{})) {
				test.Fatal("empty derived generation emitted metadata objects or nonempty directories")
			}
			for _, object := range append(objects, DocRangeObject{Bytes: encoded}) {
				for _, forbidden := range []string{`"locked"`, "opaque-locked-only", "public-message-only", "public-seed-only", "public-summary-only", "private-tool-input-only", "private-tool-output-only", "legacy-byte-large-body-only", "\ud55c🙂"} {
					if bytes.Contains(object.Bytes, []byte(forbidden)) {
						test.Fatalf("derived metadata retained event payload %q", forbidden)
					}
				}
			}
			repeated, repeatedObjects, err := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
			if err != nil || !reflect.DeepEqual(repeated, generation) || !reflect.DeepEqual(repeatedObjects, objects) {
				test.Fatalf("derived generation changed descriptor bytes or metadata object ordering on repeat: %v", err)
			}
			*generation.SourceIdentity = DocumentIdentityRootV1
			generation.Envelope[0] = '!'
			for _, object := range objects {
				object.Bytes[0] = '!'
			}
			rebuilt, rebuiltObjects, err := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
			if err != nil || !reflect.DeepEqual(rebuilt, repeated) || !reflect.DeepEqual(rebuiltObjects, repeatedObjects) {
				test.Fatalf("returned derived metadata aliases a later generation: %v", err)
			}
			unchanged, unchangedObjects, err := BuildLegacyDocRangeGeneration(context.Background(), proof)
			currentPlan, currentChunked := proof.ChunkPlan()
			if err != nil || !reflect.DeepEqual(unchanged, baseline) || !reflect.DeepEqual(unchangedObjects, baselineObjects) ||
				proof.DocumentRef() != originalRef || !bytes.Equal(proof.Bytes(), original) || currentChunked != originalChunked ||
				!reflect.DeepEqual(currentPlan, originalPlan) {
				test.Fatalf("derived generation changed the source proof, original chunk plan, or subsequent v3 output: %v", err)
			}
			if _, exists := proof.ConversationManifest(); exists {
				test.Fatal("derived byte metadata upgraded the source legacy proof to root identity")
			}
		})
	}
}

func TestLegacyDerivedDocByteRangeGenerationResolvesOnlySelectedChunkMetadata(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, DocRangePageEvents+1)
	generation, objects, err := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	if generation.EventPages.Count < 2 || generation.ChunkPages.Count != 1 {
		test.Fatal("selected metadata fixture needs unrelated event pages and one chunk descriptor page")
	}
	bodies := make(map[ContentHash][]byte, len(objects))
	for _, object := range objects {
		bodies[object.Hash] = object.Bytes
	}
	var directory docRangeDirectory
	if err := json.Unmarshal(bodies[generation.ChunkPages.Hash], &directory); err != nil || len(directory.Hashes) != 1 {
		test.Fatalf("decode fixture chunk directory: %v", err)
	}
	selectedHash := directory.Hashes[0]
	var loaded []ContentHash
	allowLeaf := false
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if hash != generation.ChunkPages.Hash && (!allowLeaf || hash != selectedHash) {
			test.Fatalf("chunk directory read unrelated event metadata, payload, or an unselected leaf: %s", hash)
		}
		loaded = append(loaded, hash)
		return docRangeGenerationLoad(bodies)(ctx, hash)
	}
	selected, err := ResolveDocRangeLeaf(context.Background(), generation.ChunkPages, 0, load)
	if err != nil || selected != selectedHash || !slices.Equal(loaded, []ContentHash{generation.ChunkPages.Hash}) {
		test.Fatalf("chunk selection loaded more than its directory path: %v, %v", loaded, err)
	}
	allowLeaf = true
	raw, err := load(context.Background(), selected)
	if err != nil {
		test.Fatal(err)
	}
	chunks, err := generation.DecodeChunks(0, selected, raw)
	if err != nil || len(chunks) != generation.ChunkCount() || !bytes.Equal(raw, docRangeGenerationJSON(test, chunks)) ||
		!slices.Equal(loaded, []ContentHash{generation.ChunkPages.Hash, selectedHash}) {
		test.Fatalf("selected chunk metadata required unrelated objects or lost canonical descriptors: %v", err)
	}
	for _, position := range []int{-1, generation.ChunkPages.Count} {
		_, err := ResolveDocRangeLeaf(context.Background(), generation.ChunkPages, position, func(context.Context, ContentHash) ([]byte, error) {
			test.Fatal("invalid chunk page selection read metadata")
			return nil, ErrNotFound
		})
		if !errors.Is(err, ErrIntegrity) {
			test.Fatalf("invalid chunk page %d was accepted: %v", position, err)
		}
	}
	corrupted := bytes.Clone(raw)
	corrupted[len(corrupted)/2] ^= 1
	if _, err := generation.DecodeChunks(0, selected, corrupted); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("selected derived metadata accepted altered bytes: %v", err)
	}
	if _, err := generation.DecodeChunks(0, HashContent([]byte("different chunk page")), raw); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("selected derived metadata accepted another leaf hash: %v", err)
	}
}

func TestLegacyDerivedDocByteRangeGenerationSourceRepresentationParity(test *testing.T) {
	materialized, original := legacyDocByteGenerationLargeFixture(test)
	baseline, baselineObjects, err := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), materialized)
	if err != nil {
		test.Fatal(err)
	}
	envelope, stream := chunkVerifierParts(test, original)
	legacyPlan, ok := PlanDocChunksV1(original)
	if !ok {
		test.Fatal("large legacy fixture did not produce an event-boundary chunk plan")
	}
	inputs := map[string]DocChunkPlan{"v1-event-boundary": legacyPlan}
	for _, width := range []int{ChunkTarget - 1, ChunkTarget + 1} {
		inputs[fmt.Sprintf("v2-width-%d", width)] = chunkVerifierPlan(envelope, stream, width)
	}
	for name, input := range inputs {
		test.Run(name, func(test *testing.T) {
			var verifier CanonicalDocVerifier
			proof, err := verifier.VerifyChunks(context.Background(), materialized.Hash(), input.Manifest, chunkVerifierLoad(input))
			if err != nil {
				test.Fatal(err)
			}
			originalPlan, originalChunked := proof.ChunkPlan()
			generation, objects, err := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
			if err != nil || !reflect.DeepEqual(generation, baseline) || !reflect.DeepEqual(objects, baselineObjects) {
				test.Fatalf("source chunk widths changed normalized derived metadata or object ordering: %v", err)
			}
			currentPlan, currentChunked := proof.ChunkPlan()
			if proof.DocumentRef() != materialized.DocumentRef() || !bytes.Equal(proof.Bytes(), original) ||
				currentChunked != originalChunked || !reflect.DeepEqual(currentPlan, originalPlan) {
				test.Fatal("derived generation changed its chunk-backed source proof")
			}
		})
	}
}

func TestLegacyDerivedDocByteRangeGenerationRejectsRootAndInvalidProof(test *testing.T) {
	legacy, original := legacyDocRangeGenerationFixture(test, 1)
	var document CIRDocument
	if err := json.Unmarshal(original, &document); err != nil {
		test.Fatal(err)
	}
	manifest, bodies, err := ConversationManifestForCIR(document)
	if err != nil {
		test.Fatal(err)
	}
	var verifier CanonicalDocVerifier
	root, err := verifier.VerifyConversationManifestDoc(context.Background(), manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies))
	if err != nil || !root.Valid() || root.DocumentRef().Identity != DocumentIdentityRootV1 {
		test.Fatalf("root proof fixture: %v", err)
	}
	for name, proof := range map[string]VerifiedSessionDoc{
		"root": root, "zero": {}, "hash-only": {hash: legacy.Hash()},
		"missing-hash":           {canonical: string(original)},
		"unknown-identity":       {hash: legacy.Hash(), canonical: string(original), identity: "future"},
		"unproved-root-identity": {hash: legacy.Hash(), canonical: string(original), identity: DocumentIdentityRootV1},
	} {
		test.Run(name, func(test *testing.T) {
			generation, objects, err := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
			if !errors.Is(err, ErrUnsupportedDocumentIdentity) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || len(objects) != 0 {
				test.Fatalf("unsupported source proof produced derived metadata: %+v, %v", generation, err)
			}
		})
	}
	if !bytes.Equal(legacy.Bytes(), original) || !bytes.Equal(root.Bytes(), original) || legacy.DocumentRef().Identity != DocumentIdentityLegacy || root.DocumentRef().Identity != DocumentIdentityRootV1 {
		test.Fatal("rejection changed either source proof or weakened its identity")
	}
}

func TestLegacyDerivedDocByteRangeGenerationCancellation(test *testing.T) {
	for _, count := range []int{0, 9} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			proof, original := legacyDocRangeGenerationFixture(test, count)
			originalRef := proof.DocumentRef()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			generation, objects, err := BuildLegacyDerivedDocByteRangeGeneration(ctx, proof)
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || len(objects) != 0 {
				test.Fatalf("pre-canceled proof produced derived metadata: %+v, %v", generation, err)
			}
			checks := 0
			baseline, baselineObjects, err := BuildLegacyDerivedDocByteRangeGeneration(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, proof)
			if err != nil || checks < count+2 {
				test.Fatalf("derived builder omitted cancellation checkpoints: %d, %v", checks, err)
			}
			for cancelAt := 1; cancelAt <= checks; cancelAt++ {
				ctx, cancel := context.WithCancel(context.Background())
				seen := 0
				_, _, err := BuildLegacyDerivedDocByteRangeGeneration(canonicalAdmissionContext{Context: ctx, check: func() {
					seen++
					if seen == cancelAt {
						cancel()
					}
				}}, proof)
				cancel()
				if !errors.Is(err, context.Canceled) {
					test.Fatalf("derived checkpoint %d ignored cancellation: %v", cancelAt, err)
				}
			}
			rebuilt, rebuiltObjects, err := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
			if err != nil || !reflect.DeepEqual(rebuilt, baseline) || !reflect.DeepEqual(rebuiltObjects, baselineObjects) ||
				proof.DocumentRef() != originalRef || !bytes.Equal(proof.Bytes(), original) {
				test.Fatalf("cancellation damaged the legacy source proof or a later derived generation: %v", err)
			}
		})
	}
}

func TestLegacyDerivedDocByteRangeGenerationByteStorageVersions(test *testing.T) {
	for _, count := range []int{0, 9} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			proof, original := legacyDocRangeGenerationFixture(test, count)
			streamed, _ := legacyDocRangeGenerationStreamFixture(test, original, ChunkTarget)
			root, _ := docRangeGenerationBuild(test, docRangeGenerationVerified(test, count))
			metadata, _, metadataErr := BuildLegacyDocRangeGeneration(context.Background(), proof)
			originalBytes, _, originalErr := BuildStreamedLegacyDocByteRangeGeneration(context.Background(), streamed, streamed.Manifest())
			derived, _, derivedErr := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
			if metadataErr != nil || originalErr != nil || derivedErr != nil {
				test.Fatalf("byte-storage fixtures: metadata=%v original=%v derived=%v", metadataErr, originalErr, derivedErr)
			}
			for _, fixture := range []struct {
				generation DocRangeGeneration
				version    int
				storage    DocRangeByteStorage
				identity   DocumentIdentity
			}{
				{root, 2, DocRangeByteStorageOriginal, DocumentIdentityRootV1},
				{metadata, 3, DocRangeByteStorageNone, DocumentIdentityLegacy},
				{originalBytes, 4, DocRangeByteStorageOriginal, DocumentIdentityLegacy},
				{derived, 5, DocRangeByteStorageDerived, DocumentIdentityLegacy},
			} {
				if fixture.generation.Version != fixture.version || fixture.generation.ByteStorage() != fixture.storage || fixture.generation.Identity() != fixture.identity {
					test.Fatalf("version %d changed byte-storage or source-identity semantics: %+v", fixture.version, fixture.generation)
				}
				encoded, err := fixture.generation.Encode()
				if err != nil {
					test.Fatal(err)
				}
				decoded, err := DecodeDocRangeGeneration(fixture.generation.Hash, HashContent(encoded), encoded)
				if err != nil || decoded.ByteStorage() != fixture.storage || decoded.Identity() != fixture.identity {
					test.Fatalf("version %d round trip weakened byte-storage or identity semantics: %v", fixture.version, err)
				}
			}
		})
	}
}

func TestLegacyDerivedDocByteRangeGenerationStrictLegacyDescriptor(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, 9)
	generation, _, err := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	for name, mutate := range map[string]func(*DocRangeGeneration){
		"zero-version":       func(value *DocRangeGeneration) { value.Version = 0 },
		"future-version":     func(value *DocRangeGeneration) { value.Version = 6 },
		"root-version":       func(value *DocRangeGeneration) { value.Version = 2 },
		"v3-with-byte-pages": func(value *DocRangeGeneration) { value.Version = 3 },
		"missing-identity":   func(value *DocRangeGeneration) { value.SourceIdentity = nil },
		"root-identity": func(value *DocRangeGeneration) {
			identity := DocumentIdentityRootV1
			value.SourceIdentity = &identity
		},
		"unknown-identity": func(value *DocRangeGeneration) {
			identity := DocumentIdentity("future")
			value.SourceIdentity = &identity
		},
		"wrong-algorithm":  func(value *DocRangeGeneration) { value.Algorithm = 2 },
		"wrong-document":   func(value *DocRangeGeneration) { value.Hash = HashContent([]byte("different document")) },
		"missing-chunks":   func(value *DocRangeGeneration) { value.ChunkPages = DocRangeDirectoryRoot{} },
		"extra-chunk-page": func(value *DocRangeGeneration) { value.ChunkPages.Count++ },
	} {
		test.Run(name, func(test *testing.T) {
			invalid := generation
			mutate(&invalid)
			if err := invalid.Validate(proof.Hash()); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("invalid v5 claim validated: %v", err)
			}
			if name != "wrong-document" {
				if raw, err := invalid.Encode(); !errors.Is(err, ErrIntegrity) || raw != nil {
					test.Fatalf("invalid v5 claim encoded: %s, %v", raw, err)
				}
			}
			if name == "unknown-identity" {
				if _, err := json.Marshal(invalid); err == nil {
					test.Fatal("unknown document identity was marshaled")
				}
				return
			}
			raw := docRangeGenerationJSON(test, invalid)
			decoded, err := DecodeDocRangeGeneration(proof.Hash(), HashContent(raw), raw)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocRangeGeneration{}) {
				test.Fatalf("invalid authenticated v5 claim decoded: %+v, %v", decoded, err)
			}
		})
	}
	for name, identity := range map[string]json.RawMessage{
		"missing": nil, "null": json.RawMessage(`null`), "root": docRangeGenerationJSON(test, DocumentIdentityRootV1),
		"unknown": json.RawMessage(`"future"`), "number": json.RawMessage(`0`), "object": json.RawMessage(`{}`),
	} {
		test.Run("wire-identity-"+name, func(test *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				test.Fatal(err)
			}
			if identity == nil {
				delete(fields, "source_identity")
			} else {
				fields["source_identity"] = identity
			}
			raw := docRangeGenerationJSON(test, fields)
			decoded, err := DecodeDocRangeGeneration(proof.Hash(), HashContent(raw), raw)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocRangeGeneration{}) {
				test.Fatalf("v5 accepted absent, implicit-root, or invalid legacy source identity: %+v, %v", decoded, err)
			}
		})
	}
	for name, raw := range map[string][]byte{
		"leading-space":      append([]byte(" "), encoded...),
		"trailing-newline":   append(bytes.Clone(encoded), '\n'),
		"duplicate-identity": bytes.Replace(encoded, []byte(`"source_identity":""`), []byte(`"source_identity":"","source_identity":""`), 1),
		"unknown-field":      append([]byte(`{"unrecognized":true,`), encoded[1:]...),
	} {
		test.Run("noncanonical-"+name, func(test *testing.T) {
			decoded, err := DecodeDocRangeGeneration(proof.Hash(), HashContent(raw), raw)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocRangeGeneration{}) {
				test.Fatalf("v5 accepted authenticated noncanonical descriptor bytes: %+v, %v", decoded, err)
			}
		})
	}
	if _, err := DecodeDocRangeGeneration(HashContent([]byte("different document")), HashContent(encoded), encoded); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("v5 accepted a different source document hash: %v", err)
	}
	if _, err := DecodeDocRangeGeneration(proof.Hash(), HashContent([]byte("different descriptor")), encoded); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("v5 accepted a different descriptor hash: %v", err)
	}
}
