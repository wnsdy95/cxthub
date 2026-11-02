package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func legacyDocByteGenerationLargeFixture(test testing.TB) (VerifiedSessionDoc, []byte) {
	test.Helper()
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{"original-model"}}, Events: []CIREvent{
		{Kind: EventMessage, Seq: 93, Role: RoleAssistant, AgentMessage: true,
			Blocks: []ContentBlock{{Type: "text", Text: "legacy-byte-large-body-only" + strings.Repeat("x", 1100<<10)}}},
	}}
	raw, err := CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	verified, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: document})
	if err != nil || !verified.Valid() || verified.DocumentRef().Identity != DocumentIdentityLegacy {
		test.Fatalf("large legacy byte fixture verification: %v", err)
	}
	return verified, raw
}

func legacyDocByteGenerationEntries(test testing.TB, generation DocRangeGeneration, objects []DocRangeObject, proof VerifiedLegacyStream, original DocChunkPlan) []DocRangeEntry {
	test.Helper()
	bodies := make(map[ContentHash][]byte, len(objects))
	for _, object := range objects {
		if len(object.Bytes) == 0 || len(object.Bytes) > MaxDocRangePageBytes || HashContent(object.Bytes) != object.Hash {
			test.Fatalf("invalid legacy byte metadata object %s (%d bytes)", object.Hash, len(object.Bytes))
		}
		if previous, exists := bodies[object.Hash]; exists && !bytes.Equal(previous, object.Bytes) {
			test.Fatalf("inconsistent legacy byte metadata object %s", object.Hash)
		}
		bodies[object.Hash] = object.Bytes
	}
	visited := make(map[ContentHash]bool, len(bodies))
	loadBody := docRangeGenerationLoad(bodies)
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		raw, err := loadBody(ctx, hash)
		if err == nil {
			visited[hash] = true
		}
		return raw, err
	}
	entries := make([]DocRangeEntry, 0, int(generation.EventCount))
	for position := 0; position < generation.EventPages.Count; position++ {
		selected, err := ResolveDocRangeLeaf(context.Background(), generation.EventPages, position, load)
		if err != nil {
			test.Fatalf("resolve legacy byte event page %d: %v", position, err)
		}
		raw, err := load(context.Background(), selected)
		if err != nil {
			test.Fatal(err)
		}
		page, err := generation.DecodePage(position, selected, raw)
		if err != nil {
			test.Fatalf("decode legacy byte event page %d: %v", position, err)
		}
		entries = append(entries, page...)
	}
	chunks := make([]ConversationManifestChunk, 0, generation.ChunkCount())
	for position := 0; position < generation.ChunkPages.Count; position++ {
		selected, err := ResolveDocRangeLeaf(context.Background(), generation.ChunkPages, position, load)
		if err != nil {
			test.Fatalf("resolve legacy byte chunk page %d: %v", position, err)
		}
		raw, err := load(context.Background(), selected)
		if err != nil {
			test.Fatal(err)
		}
		page, err := generation.DecodeChunks(position, selected, raw)
		if err != nil || !bytes.Equal(raw, docRangeGenerationJSON(test, page)) {
			test.Fatalf("legacy byte chunk page %d is not ordinary canonical conversation chunk metadata: %v", position, err)
		}
		chunks = append(chunks, page...)
	}
	if len(chunks) != generation.ChunkCount() || len(chunks) != len(original.Manifest.Chunks) {
		test.Fatalf("legacy byte chunk partition changed: got %d, generation=%d original=%d", len(chunks), generation.ChunkCount(), len(original.Manifest.Chunks))
	}
	var streamBytes int64
	for ordinal, chunk := range chunks {
		body, err := proof.Chunk(context.Background(), ordinal)
		if err != nil {
			test.Fatalf("read verified legacy chunk %d: %v", ordinal, err)
		}
		expectedBytes := min(int64(512<<10), generation.StreamBytes-int64(ordinal)*(512<<10))
		if chunk.Hash != original.Manifest.Chunks[ordinal] || chunk.Hash != HashContent(body) ||
			chunk.Bytes != int64(len(body)) || chunk.Bytes != expectedBytes || !bytes.Equal(body, original.Bodies[chunk.Hash]) {
			test.Fatalf("legacy byte chunk %d changed original hash, length, or bytes: %+v (%d bytes)", ordinal, chunk, len(body))
		}
		if _, exists := bodies[chunk.Hash]; exists {
			test.Fatalf("legacy byte generation emitted raw original chunk %d", ordinal)
		}
		streamBytes += chunk.Bytes
	}
	if streamBytes != generation.StreamBytes || len(entries) != int(generation.EventCount) || len(visited) != len(bodies) {
		test.Fatalf("legacy byte generation has wrong stream extent, event count, or unreachable objects: bytes=%d events=%d visited=%d objects=%d", streamBytes, len(entries), len(visited), len(bodies))
	}
	return entries
}

func TestStreamedLegacyDocByteRangeGenerationOriginalChunksAndMetadata(test *testing.T) {
	for _, fixture := range []struct {
		name       string
		count      int
		large      bool
		wantChunks int
	}{
		{name: "empty", count: 0, wantChunks: 0},
		{name: "small", count: 9, wantChunks: 1},
		{name: "multiple-event-pages", count: DocRangePageEvents + 1, wantChunks: 1},
		{name: "three-original-chunks", large: true, wantChunks: 3},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var materialized VerifiedSessionDoc
			var raw []byte
			if fixture.large {
				materialized, raw = legacyDocByteGenerationLargeFixture(test)
			} else {
				materialized, raw = legacyDocRangeGenerationFixture(test, fixture.count)
			}
			envelope, stream := chunkVerifierParts(test, raw)
			original := chunkVerifierPlan(envelope, stream, ChunkTarget)
			proof, spool := legacyDocRangeGenerationStreamFixture(test, raw, ChunkTarget)
			originalRef, normalized := proof.Reference().DocumentRef(), proof.Manifest()
			originalStream := bytes.Clone(spool.body)
			baseline, baselineObjects, err := BuildLegacyDocRangeGeneration(context.Background(), materialized)
			if err != nil {
				test.Fatal(err)
			}
			baselineBytes, err := baseline.Encode()
			if err != nil {
				test.Fatal(err)
			}
			streamed, streamedObjects, err := BuildStreamedLegacyDocRangeGeneration(context.Background(), proof)
			if err != nil || baseline.Version != 3 || baseline.ChunkCount() != 0 || baseline.ChunkPages != (DocRangeDirectoryRoot{}) ||
				!reflect.DeepEqual(streamed, baseline) || !reflect.DeepEqual(streamedObjects, baselineObjects) {
				test.Fatalf("existing legacy builders changed their exact v3 output: %v", err)
			}
			wantEntries := legacyDocRangeGenerationPages(test, baseline, baselineObjects)
			generation, objects, err := BuildStreamedLegacyDocByteRangeGeneration(context.Background(), proof, original.Manifest)
			if err != nil {
				test.Fatal(err)
			}
			if generation.Version != 4 || generation.SourceIdentity == nil || *generation.SourceIdentity != DocumentIdentityLegacy ||
				generation.Identity() != DocumentIdentityLegacy || generation.Hash != HashContent(raw) || generation.Algorithm != 1 ||
				!bytes.Equal(generation.Envelope, envelope) || generation.StreamBytes != int64(len(stream)) ||
				generation.ChunkCount() != fixture.wantChunks || generation.ChunkCount() != (len(stream)+(512<<10)-1)/(512<<10) ||
				generation.ChunkPages.Count != (fixture.wantChunks+DocRangeChunkPageSize-1)/DocRangeChunkPageSize {
				test.Fatalf("legacy byte generation changed original identity, metadata, or fixed chunk partition: %+v", generation)
			}
			metadataOnly := generation
			metadataOnly.Version, metadataOnly.ChunkPages = 3, DocRangeDirectoryRoot{}
			metadataBytes, err := metadataOnly.Encode()
			if err != nil || !reflect.DeepEqual(metadataOnly, baseline) || !bytes.Equal(metadataBytes, baselineBytes) {
				test.Fatalf("v4 rewrote existing v3 event metadata: %v", err)
			}
			encoded, err := generation.Encode()
			if err != nil || len(encoded) > MaxDocRangeGenerationBytes || !bytes.Contains(encoded, []byte(`"source_identity":""`)) ||
				!bytes.Equal(encoded, docRangeGenerationJSON(test, generation)) {
				test.Fatalf("legacy byte descriptor is not canonical: %v", err)
			}
			decoded, err := DecodeDocRangeGeneration(materialized.Hash(), HashContent(encoded), encoded)
			if err != nil || !reflect.DeepEqual(decoded, generation) {
				test.Fatalf("legacy byte descriptor round trip: %v", err)
			}
			reencoded, err := decoded.Encode()
			if err != nil || !bytes.Equal(encoded, reencoded) {
				test.Fatalf("legacy byte descriptor re-encoding changed canonical bytes: %v", err)
			}
			entries := legacyDocByteGenerationEntries(test, decoded, objects, proof, original)
			if !reflect.DeepEqual(entries, wantEntries) {
				test.Fatal("legacy byte generation changed original event locations, hashes, sequences, or presentation metadata")
			}
			if fixture.wantChunks == 0 && (len(objects) != 0 || generation.ChunkPages != (DocRangeDirectoryRoot{})) {
				test.Fatal("empty legacy stream emitted byte objects or a nonempty chunk directory")
			}
			for _, object := range append(objects, DocRangeObject{Bytes: encoded}) {
				for _, forbidden := range []string{`"locked"`, "opaque-locked-only", "public-message-only", "public-seed-only", "public-summary-only", "private-tool-input-only", "private-tool-output-only", "legacy-byte-large-body-only"} {
					if bytes.Contains(object.Bytes, []byte(forbidden)) {
						test.Fatalf("legacy byte generation emitted original event payload %q", forbidden)
					}
				}
			}
			repeated, repeatedObjects, err := BuildStreamedLegacyDocByteRangeGeneration(context.Background(), proof, original.Manifest)
			if err != nil || !reflect.DeepEqual(repeated, generation) || !reflect.DeepEqual(repeatedObjects, objects) {
				test.Fatalf("legacy byte generation is nondeterministic: %v", err)
			}
			for _, build := range []func() (DocRangeGeneration, []DocRangeObject, error){
				func() (DocRangeGeneration, []DocRangeObject, error) {
					return BuildLegacyDocRangeGeneration(context.Background(), materialized)
				},
				func() (DocRangeGeneration, []DocRangeObject, error) {
					return BuildStreamedLegacyDocRangeGeneration(context.Background(), proof)
				},
			} {
				unchanged, unchangedObjects, err := build()
				if err != nil || !reflect.DeepEqual(unchanged, baseline) || !reflect.DeepEqual(unchangedObjects, baselineObjects) {
					test.Fatalf("byte enrichment changed a subsequent v3 build: %v", err)
				}
			}
			if proof.Reference().DocumentRef() != originalRef || originalRef != materialized.DocumentRef() ||
				!reflect.DeepEqual(proof.Manifest(), normalized) || !reflect.DeepEqual(original.Manifest, normalized) ||
				!bytes.Equal(spool.body, originalStream) || !bytes.Equal(materialized.Bytes(), raw) {
				test.Fatal("legacy byte generation rewrote the source proof, original manifest, spool, or document")
			}
		})
	}
}

func TestStreamedLegacyDocByteRangeGenerationRequiresExactOriginalManifestAndExtent(test *testing.T) {
	_, raw := legacyDocRangeGenerationFixture(test, 9)
	valid, spool := legacyDocRangeGenerationStreamFixture(test, raw, ChunkTarget)
	baseline, baselineObjects, err := BuildStreamedLegacyDocRangeGeneration(context.Background(), valid)
	if err != nil {
		test.Fatal(err)
	}
	baselineBytes, err := baseline.Encode()
	if err != nil {
		test.Fatal(err)
	}
	originalStream := bytes.Clone(spool.body)
	for name, mutate := range map[string]func(*VerifiedLegacyStream, *DocChunkManifest){
		"v1-format":          func(proof *VerifiedLegacyStream, manifest *DocChunkManifest) { manifest.Format = ChunkFormatV1 },
		"unversioned-format": func(proof *VerifiedLegacyStream, manifest *DocChunkManifest) { manifest.Format = "" },
		"unknown-format":     func(proof *VerifiedLegacyStream, manifest *DocChunkManifest) { manifest.Format = "future" },
		"missing-envelope":   func(proof *VerifiedLegacyStream, manifest *DocChunkManifest) { manifest.Envelope = nil },
		"different-envelope": func(proof *VerifiedLegacyStream, manifest *DocChunkManifest) {
			manifest.Envelope = docRangeGenerationJSON(test, CIREnvelope{CIRVersion: "2", SourceModels: []string{"different-model"}})
		},
		"equivalent-noncanonical-envelope": func(proof *VerifiedLegacyStream, manifest *DocChunkManifest) {
			manifest.Envelope = append(manifest.Envelope, ' ')
		},
		"wrong-chunk-hash": func(proof *VerifiedLegacyStream, manifest *DocChunkManifest) {
			manifest.Chunks[0] = HashContent([]byte("different original chunk"))
		},
		"missing-chunks": func(proof *VerifiedLegacyStream, manifest *DocChunkManifest) { manifest.Chunks = nil },
		"extra-chunk": func(proof *VerifiedLegacyStream, manifest *DocChunkManifest) {
			manifest.Chunks = append(manifest.Chunks, manifest.Chunks[0])
		},
		"short-source-extent": func(proof *VerifiedLegacyStream, manifest *DocChunkManifest) { proof.size-- },
		"long-source-extent":  func(proof *VerifiedLegacyStream, manifest *DocChunkManifest) { proof.size++ },
	} {
		test.Run(name, func(test *testing.T) {
			proof, manifest := valid, valid.Manifest()
			mutate(&proof, &manifest)
			generation, objects, err := BuildStreamedLegacyDocByteRangeGeneration(context.Background(), proof, manifest)
			if err != nil || !reflect.DeepEqual(generation, baseline) || !reflect.DeepEqual(objects, baselineObjects) ||
				generation.Version != 3 || generation.ChunkCount() != 0 || generation.ChunkPages != (DocRangeDirectoryRoot{}) {
				test.Fatalf("non-original bytes were advertised as ready instead of exact v3 fallback: %+v, %v", generation, err)
			}
			encoded, err := generation.Encode()
			if err != nil || !bytes.Equal(encoded, baselineBytes) || !bytes.Equal(spool.body, originalStream) {
				test.Fatalf("v3 fallback rewrote descriptor or source bytes: %v", err)
			}
		})
	}
}

func TestStreamedLegacyDocByteRangeGenerationNonNormalizedOriginalWidthsRemainV3(test *testing.T) {
	_, small := legacyDocRangeGenerationFixture(test, 9)
	_, large := legacyDocByteGenerationLargeFixture(test)
	for _, fixture := range []struct {
		name  string
		raw   []byte
		width int
	}{
		{name: "narrow", raw: small, width: 31},
		{name: "one-byte-short", raw: large, width: ChunkTarget - 1},
		{name: "one-byte-long", raw: large, width: ChunkTarget + 1},
		{name: "whole-large-event", raw: large, width: 4 * ChunkTarget},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			envelope, stream := chunkVerifierParts(test, fixture.raw)
			original := chunkVerifierPlan(envelope, stream, fixture.width)
			proof, spool := legacyDocRangeGenerationStreamFixture(test, fixture.raw, fixture.width)
			if slices.Equal(original.Manifest.Chunks, proof.Manifest().Chunks) {
				test.Fatal("non-normalized fixture unexpectedly has normalized original chunk hashes")
			}
			baseline, baselineObjects, err := BuildStreamedLegacyDocRangeGeneration(context.Background(), proof)
			if err != nil {
				test.Fatal(err)
			}
			generation, objects, err := BuildStreamedLegacyDocByteRangeGeneration(context.Background(), proof, original.Manifest)
			if err != nil || generation.Version != 3 || generation.ChunkCount() != 0 || generation.ChunkPages != (DocRangeDirectoryRoot{}) ||
				!reflect.DeepEqual(generation, baseline) || !reflect.DeepEqual(objects, baselineObjects) || !bytes.Equal(spool.body, stream) {
				test.Fatalf("non-normalized width %d pretended normalized bytes already existed: %v", fixture.width, err)
			}
			legacyDocRangeGenerationPages(test, generation, objects)
		})
	}
	proof, _ := legacyDocRangeGenerationStreamFixture(test, large, ChunkTarget)
	baseline, baselineObjects, err := BuildStreamedLegacyDocRangeGeneration(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	original := proof.Manifest()
	slices.Reverse(original.Chunks)
	if slices.Equal(original.Chunks, proof.Manifest().Chunks) {
		test.Fatal("reordered original fixture did not change chunk order")
	}
	generation, objects, err := BuildStreamedLegacyDocByteRangeGeneration(context.Background(), proof, original)
	if err != nil || !reflect.DeepEqual(generation, baseline) || !reflect.DeepEqual(objects, baselineObjects) {
		test.Fatalf("reordered original chunks did not preserve exact v3 output: %v", err)
	}
}

func TestStreamedLegacyDocByteRangeGenerationDescriptorRejectsUnknownVersionsAndIdentities(test *testing.T) {
	_, raw := legacyDocRangeGenerationFixture(test, 9)
	proof, _ := legacyDocRangeGenerationStreamFixture(test, raw, ChunkTarget)
	generation, _, err := BuildStreamedLegacyDocByteRangeGeneration(context.Background(), proof, proof.Manifest())
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	for name, mutate := range map[string]func(*DocRangeGeneration){
		"zero-version":     func(value *DocRangeGeneration) { value.Version = 0 },
		"unknown-version":  func(value *DocRangeGeneration) { value.Version = 6 },
		"root-version":     func(value *DocRangeGeneration) { value.Version = 2 },
		"v3-with-chunks":   func(value *DocRangeGeneration) { value.Version = 3 },
		"missing-identity": func(value *DocRangeGeneration) { value.SourceIdentity = nil },
		"root-identity": func(value *DocRangeGeneration) {
			identity := DocumentIdentityRootV1
			value.SourceIdentity = &identity
		},
		"unknown-identity": func(value *DocRangeGeneration) {
			identity := DocumentIdentity("future")
			value.SourceIdentity = &identity
		},
	} {
		test.Run(name, func(test *testing.T) {
			invalid := generation
			mutate(&invalid)
			if err := invalid.Validate(generation.Hash); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("invalid legacy byte descriptor validated: %v", err)
			}
			if body, err := invalid.Encode(); !errors.Is(err, ErrIntegrity) || body != nil {
				test.Fatalf("invalid legacy byte descriptor encoded: %s, %v", body, err)
			}
			var body []byte
			if name == "unknown-identity" {
				body = bytes.Replace(encoded, []byte(`"source_identity":""`), []byte(`"source_identity":"future"`), 1)
			} else {
				body = docRangeGenerationJSON(test, invalid)
			}
			decoded, err := DecodeDocRangeGeneration(generation.Hash, HashContent(body), body)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocRangeGeneration{}) {
				test.Fatalf("invalid legacy byte descriptor decoded: %+v, %v", decoded, err)
			}
		})
	}
	for name, body := range map[string][]byte{
		"noncanonical":        append([]byte{' '}, encoded...),
		"null-identity":       bytes.Replace(encoded, []byte(`"source_identity":""`), []byte(`"source_identity":null`), 1),
		"wrong-identity-type": bytes.Replace(encoded, []byte(`"source_identity":""`), []byte(`"source_identity":0`), 1),
	} {
		test.Run(name, func(test *testing.T) {
			if _, err := DecodeDocRangeGeneration(generation.Hash, HashContent(body), body); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("invalid legacy byte descriptor wire representation accepted: %v", err)
			}
		})
	}
	if _, err := DecodeDocRangeGeneration(HashContent([]byte("different legacy document")), HashContent(encoded), encoded); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("legacy byte descriptor accepted a different source document: %v", err)
	}
	if _, err := DecodeDocRangeGeneration(generation.Hash, HashContent([]byte("different descriptor")), encoded); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("legacy byte descriptor accepted a different descriptor hash: %v", err)
	}
}

func TestStreamedLegacyDocByteRangeGenerationSelectedChunksRejectCorruption(test *testing.T) {
	_, raw := legacyDocByteGenerationLargeFixture(test)
	proof, _ := legacyDocRangeGenerationStreamFixture(test, raw, ChunkTarget)
	generation, objects, err := BuildStreamedLegacyDocByteRangeGeneration(context.Background(), proof, proof.Manifest())
	if err != nil {
		test.Fatal(err)
	}
	bodies := make(map[ContentHash][]byte, len(objects))
	for _, object := range objects {
		bodies[object.Hash] = object.Bytes
	}
	load := docRangeGenerationLoad(bodies)
	selected, err := ResolveDocRangeLeaf(context.Background(), generation.ChunkPages, 0, load)
	if err != nil {
		test.Fatal(err)
	}
	page := bodies[selected]
	chunks, err := generation.DecodeChunks(0, selected, page)
	if err != nil || len(chunks) != 3 {
		test.Fatalf("expected three selected legacy byte chunk references: %v", err)
	}
	corrupt := bytes.Clone(page)
	corrupt[len(corrupt)/2] ^= 1
	wrongChunks := slices.Clone(chunks)
	wrongChunks[0].Hash = HashContent([]byte("wrong original chunk"))
	for name, body := range map[string][]byte{
		"corrupt": corrupt, "different-valid-page": docRangeGenerationJSON(test, wrongChunks), "missing": nil,
	} {
		test.Run(name, func(test *testing.T) {
			if _, err := generation.DecodeChunks(0, selected, body); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("changed selected legacy byte chunk page accepted: %v", err)
			}
		})
	}
	if _, err := generation.DecodeChunks(0, HashContent([]byte("wrong selected page")), page); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("legacy byte chunk page accepted a wrong selected hash: %v", err)
	}
	for name, mutate := range map[string]func([]ConversationManifestChunk) []ConversationManifestChunk{
		"missing-chunk": func(value []ConversationManifestChunk) []ConversationManifestChunk { return value[:len(value)-1] },
		"extra-chunk":   func(value []ConversationManifestChunk) []ConversationManifestChunk { return append(value, value[0]) },
		"invalid-hash": func(value []ConversationManifestChunk) []ConversationManifestChunk {
			value[0].Hash = "invalid"
			return value
		},
		"short-full-chunk": func(value []ConversationManifestChunk) []ConversationManifestChunk {
			value[0].Bytes--
			return value
		},
		"long-final-chunk": func(value []ConversationManifestChunk) []ConversationManifestChunk {
			value[len(value)-1].Bytes++
			return value
		},
		"zero-final-chunk": func(value []ConversationManifestChunk) []ConversationManifestChunk {
			value[len(value)-1].Bytes = 0
			return value
		},
	} {
		test.Run(name, func(test *testing.T) {
			body := docRangeGenerationJSON(test, mutate(slices.Clone(chunks)))
			if _, err := generation.DecodeChunks(0, HashContent(body), body); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("authenticated but invalid legacy byte chunk page accepted: %v", err)
			}
		})
	}
	for name, body := range map[string][]byte{
		"empty": nil, "null": []byte(`null`), "malformed": []byte(`[`), "oversized": bytes.Repeat([]byte{' '}, MaxDocRangeDirectoryBytes+1),
	} {
		test.Run(name+"-authenticated", func(test *testing.T) {
			if _, err := generation.DecodeChunks(0, HashContent(body), body); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("invalid legacy byte chunk page representation accepted: %v", err)
			}
		})
	}
	for _, position := range []int{-1, generation.ChunkPages.Count} {
		if _, err := ResolveDocRangeLeaf(context.Background(), generation.ChunkPages, position, load); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("invalid legacy byte chunk directory position %d accepted: %v", position, err)
		}
		if _, err := generation.DecodeChunks(position, selected, page); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("invalid legacy byte chunk page position %d accepted: %v", position, err)
		}
	}
	if _, err := ResolveDocRangeLeaf(context.Background(), generation.ChunkPages, 0, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		body, err := load(ctx, hash)
		if err != nil {
			return nil, err
		}
		changed := bytes.Clone(body)
		changed[0] ^= 1
		return changed, nil
	}); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("corrupt legacy byte chunk directory accepted: %v", err)
	}
}

func TestStreamedLegacyDocByteRangeGenerationCancellation(test *testing.T) {
	for _, count := range []int{0, 9} {
		for _, fallback := range []bool{false, true} {
			test.Run(fmt.Sprintf("events-%d/fallback-%t", count, fallback), func(test *testing.T) {
				_, raw := legacyDocRangeGenerationFixture(test, count)
				proof, spool := legacyDocRangeGenerationStreamFixture(test, raw, ChunkTarget)
				original := proof.Manifest()
				if fallback {
					original.Format = ChunkFormatV1
				}
				originalRef, normalized := proof.Reference().DocumentRef(), proof.Manifest()
				originalStream := bytes.Clone(spool.body)
				checks := 0
				baseline, baselineObjects, err := BuildStreamedLegacyDocByteRangeGeneration(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, proof, original)
				if err != nil || checks == 0 {
					test.Fatalf("missing legacy byte generation cancellation checkpoints: %d, %v", checks, err)
				}
				for cancelAt := 1; cancelAt <= checks; cancelAt++ {
					ctx, cancel := context.WithCancel(context.Background())
					seen := 0
					_, _, err := BuildStreamedLegacyDocByteRangeGeneration(canonicalAdmissionContext{Context: ctx, check: func() {
						seen++
						if seen == cancelAt {
							cancel()
						}
					}}, proof, original)
					cancel()
					if !errors.Is(err, context.Canceled) {
						test.Fatalf("legacy byte generation checkpoint %d ignored cancellation: %v", cancelAt, err)
					}
				}
				rebuilt, rebuiltObjects, err := BuildStreamedLegacyDocByteRangeGeneration(context.Background(), proof, original)
				if err != nil || !reflect.DeepEqual(rebuilt, baseline) || !reflect.DeepEqual(rebuiltObjects, baselineObjects) ||
					proof.Reference().DocumentRef() != originalRef || !reflect.DeepEqual(proof.Manifest(), normalized) || !bytes.Equal(spool.body, originalStream) {
					test.Fatalf("canceled legacy byte generation damaged the original proof, spool, or retry: %v", err)
				}
			})
		}
	}
}

func TestStreamedLegacyDocByteRangeGenerationRejectsInvalidProof(test *testing.T) {
	_, raw := legacyDocRangeGenerationFixture(test, 1)
	valid, spool := legacyDocRangeGenerationStreamFixture(test, raw, ChunkTarget)
	original := valid.Manifest()
	spool.readTransform = func(body []byte) []byte {
		test.Fatal("invalid legacy byte proof read source bytes")
		return body
	}
	for name, invalidate := range map[string]func(*VerifiedLegacyStream){
		"zero":              func(proof *VerifiedLegacyStream) { *proof = VerifiedLegacyStream{} },
		"missing-source":    func(proof *VerifiedLegacyStream) { proof.source = nil },
		"missing-reference": func(proof *VerifiedLegacyStream) { proof.reference = VerifiedDocReference{} },
		"wrong-format":      func(proof *VerifiedLegacyStream) { proof.manifest.Format = ChunkFormatV1 },
		"root-identity":     func(proof *VerifiedLegacyStream) { proof.reference.identity = DocumentIdentityRootV1 },
		"unknown-identity":  func(proof *VerifiedLegacyStream) { proof.reference.identity = "future" },
	} {
		test.Run(name, func(test *testing.T) {
			proof := valid
			invalidate(&proof)
			generation, objects, err := BuildStreamedLegacyDocByteRangeGeneration(context.Background(), proof, original)
			if !errors.Is(err, ErrUnsupportedDocumentIdentity) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || len(objects) != 0 {
				test.Fatalf("invalid legacy byte proof produced metadata: %+v, %v", generation, err)
			}
		})
	}
}
