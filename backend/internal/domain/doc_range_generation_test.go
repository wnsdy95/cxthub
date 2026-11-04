package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func docRangeGenerationJSON(test testing.TB, value any) []byte {
	test.Helper()
	raw, err := canonicalJSON(value)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func docRangeGenerationVerified(test *testing.T, count int) VerifiedDocRangeIndex {
	test.Helper()
	manifest, bodies, err := ConversationManifestForCIR(CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}, Events: canonicalAdmissionEvents(0, count)})
	if err != nil {
		test.Fatal(err)
	}
	var verifier CanonicalDocVerifier
	verified, err := verifier.VerifyConversationRangeIndex(context.Background(), manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies), &docRangeMemoryBuffer{})
	if err != nil {
		test.Fatal(err)
	}
	return verified
}

func docRangeGenerationBuild(test testing.TB, verified VerifiedDocRangeIndex) (DocRangeGeneration, []DocRangeObject) {
	test.Helper()
	generation, objects, err := BuildDocRangeGeneration(context.Background(), verified)
	if err != nil {
		test.Fatal(err)
	}
	if err := generation.Validate(verified.Header().Hash); err != nil {
		test.Fatal(err)
	}
	return generation, objects
}

func docRangeGenerationObjects(test testing.TB, objects []DocRangeObject) map[ContentHash][]byte {
	test.Helper()
	bodies := make(map[ContentHash][]byte, len(objects))
	for _, object := range objects {
		if HashContent(object.Bytes) != object.Hash || len(object.Bytes) == 0 || len(object.Bytes) > MaxDocRangeDirectoryBytes {
			test.Fatalf("invalid generated object %s (%d bytes)", object.Hash, len(object.Bytes))
		}
		if previous, exists := bodies[object.Hash]; exists && !bytes.Equal(previous, object.Bytes) {
			test.Fatalf("inconsistent duplicate object %s", object.Hash)
		}
		bodies[object.Hash] = object.Bytes
	}
	return bodies
}

func docRangeGenerationLoad(bodies map[ContentHash][]byte) func(context.Context, ContentHash) ([]byte, error) {
	return func(ctx context.Context, hash ContentHash) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, exists := bodies[hash]
		if !exists {
			return nil, ErrNotFound
		}
		return raw, nil
	}
}

func TestDocRangeGenerationBuildDeterministicVerifiedParity(test *testing.T) {
	for _, count := range []int{0, 1, DocRangePageEvents, DocRangePageEvents + 1, 3*DocRangePageEvents + 7} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			verified := docRangeGenerationVerified(test, count)
			header := verified.Header()
			generation, objects := docRangeGenerationBuild(test, verified)
			repeated, repeatedObjects := docRangeGenerationBuild(test, verified)
			if !reflect.DeepEqual(generation, repeated) || !reflect.DeepEqual(objects, repeatedObjects) {
				test.Fatal("same verified proof produced different generation or object ordering")
			}
			if generation.Version != 2 || generation.Algorithm != 1 || generation.Hash != header.Hash ||
				!bytes.Equal(generation.Envelope, header.Manifest.Envelope) || generation.EventCount != int64(count) ||
				generation.StreamBytes != header.Manifest.StreamBytes || generation.ChunkCount() != len(header.Manifest.Chunks) ||
				generation.EventPages.Count != len(header.Pages) {
				test.Fatalf("generation changed verified metadata: %+v", generation)
			}
			raw, err := generation.Encode()
			if err != nil {
				test.Fatal(err)
			}
			decoded, err := DecodeDocRangeGeneration(header.Hash, HashContent(raw), raw)
			if err != nil || !reflect.DeepEqual(decoded, generation) {
				test.Fatalf("descriptor round trip: %v", err)
			}
			bodies := docRangeGenerationObjects(test, objects)
			for position, expected := range header.Pages {
				selected, err := ResolveDocRangeLeaf(context.Background(), generation.EventPages, position, docRangeGenerationLoad(bodies))
				if err != nil || selected != expected {
					test.Fatalf("event page %d: %s, %v; want %s", position, selected, err, expected)
				}
				page := docRangeTestPage(test, verified, position)
				want, err := header.DecodePage(position, page)
				if err != nil {
					test.Fatal(err)
				}
				got, err := decoded.DecodePage(position, selected, page)
				if err != nil || !reflect.DeepEqual(got, want) {
					test.Fatalf("event page %d changed: %v", position, err)
				}
			}
			chunks := make([]ConversationManifestChunk, 0)
			for position := 0; position < generation.ChunkPages.Count; position++ {
				selected, err := ResolveDocRangeLeaf(context.Background(), generation.ChunkPages, position, docRangeGenerationLoad(bodies))
				if err != nil {
					test.Fatal(err)
				}
				page, err := generation.DecodeChunks(position, selected, bodies[selected])
				if err != nil {
					test.Fatal(err)
				}
				chunks = append(chunks, page...)
			}
			if !slices.Equal(chunks, header.Manifest.Chunks) {
				test.Fatal("chunk descriptors changed order or partition")
			}
			if count == 0 && (len(objects) != 0 || generation.EventPages != (DocRangeDirectoryRoot{}) || generation.ChunkPages != (DocRangeDirectoryRoot{})) {
				test.Fatal("empty document created directory objects")
			}
		})
	}
}

func TestDocRangeGenerationBuildFromExistingMultiChunkFixture(test *testing.T) {
	manifest, bodies, _ := rootEventsFixture(test)
	var verifier CanonicalDocVerifier
	hash := manifestProofHash(test, manifest)
	verified, err := verifier.VerifyConversationRangeIndex(context.Background(), hash, manifest, manifestProofLoad(bodies), &docRangeMemoryBuffer{})
	if err != nil {
		test.Fatal(err)
	}
	document, err := verifier.VerifyConversationManifestDoc(context.Background(), hash, manifest, manifestProofLoad(bodies))
	if err != nil {
		test.Fatal(err)
	}
	materialized, err := BuildDocRangeIndex(context.Background(), document)
	if err != nil {
		test.Fatal(err)
	}
	generation, objects := docRangeGenerationBuild(test, verified)
	other, otherObjects := docRangeGenerationBuild(test, materialized)
	if !reflect.DeepEqual(generation, other) || !reflect.DeepEqual(objects, otherObjects) {
		test.Fatal("streaming and materialized verification produced different generations")
	}
	stored := docRangeGenerationObjects(test, objects)
	selected, err := ResolveDocRangeLeaf(context.Background(), generation.ChunkPages, 0, docRangeGenerationLoad(stored))
	if err != nil {
		test.Fatal(err)
	}
	chunks, err := generation.DecodeChunks(0, selected, stored[selected])
	if err != nil || !slices.Equal(chunks, manifest.Chunks) || len(chunks) < 2 || chunks[len(chunks)-1].Bytes == ConversationManifestChunkBytes {
		test.Fatalf("multi-chunk fixture lost final partial chunk: %v", err)
	}
}

func TestDocRangeGenerationRejectsZeroProofAndCancellation(test *testing.T) {
	verified := docRangeGenerationVerified(test, DocRangePageEvents+1)
	for _, proof := range []VerifiedDocRangeIndex{{}, {index: verified.Header()}} {
		generation, objects, err := BuildDocRangeGeneration(context.Background(), proof)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || len(objects) != 0 {
			test.Fatalf("unverified header promoted: %+v, %v", generation, err)
		}
	}
	checks := 0
	_, _, err := BuildDocRangeGeneration(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, verified)
	if err != nil || checks < 4 {
		test.Fatalf("baseline cancellation checkpoints: %d, %v", checks, err)
	}
	for cancelAt := 1; cancelAt <= checks; cancelAt++ {
		test.Run(fmt.Sprint(cancelAt), func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			seen := 0
			_, _, err := BuildDocRangeGeneration(canonicalAdmissionContext{Context: ctx, check: func() {
				seen++
				if seen == cancelAt {
					cancel()
				}
			}}, verified)
			if !errors.Is(err, context.Canceled) {
				test.Fatalf("checkpoint %d returned %v", cancelAt, err)
			}
		})
	}
}

func TestDocRangeGenerationOwnsDescriptorAndObjectBytes(test *testing.T) {
	verified := docRangeGenerationVerified(test, DocRangePageEvents+1)
	generation, objects := docRangeGenerationBuild(test, verified)
	baseline, baselineObjects := docRangeGenerationBuild(test, verified)
	raw, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	encoded := bytes.Clone(raw)
	decoded, err := DecodeDocRangeGeneration(generation.Hash, HashContent(raw), raw)
	if err != nil {
		test.Fatal(err)
	}
	for position := range raw {
		raw[position] = 'x'
	}
	if !reflect.DeepEqual(decoded, baseline) {
		test.Fatal("decoder retained caller-owned descriptor bytes")
	}
	decoded.Envelope[0] = 'x'
	generation.Envelope[0] = 'x'
	for position := range objects {
		objects[position].Bytes[0] = 'x'
	}
	header := verified.Header()
	header.Manifest.Envelope[0] = 'x'
	header.Pages[0] = HashContent([]byte("caller replacement"))
	header.Manifest.Chunks[0].Bytes = 0
	rebuilt, rebuiltObjects := docRangeGenerationBuild(test, verified)
	if !reflect.DeepEqual(rebuilt, baseline) || !reflect.DeepEqual(rebuiltObjects, baselineObjects) {
		test.Fatal("returned bytes or header aliases changed the verified proof or another build")
	}
	if _, err := DecodeDocRangeGeneration(baseline.Hash, HashContent(encoded), encoded); err != nil {
		test.Fatalf("mutating descriptor changed previously encoded bytes: %v", err)
	}
}

func TestDocRangeGenerationDescriptorRejectsInvalidClaims(test *testing.T) {
	valid, _ := docRangeGenerationBuild(test, docRangeGenerationVerified(test, 1))
	otherHash := HashContent([]byte("another document"))
	for name, mutate := range map[string]func(*DocRangeGeneration){
		"version-zero":       func(value *DocRangeGeneration) { value.Version = 0 },
		"version-old":        func(value *DocRangeGeneration) { value.Version = 1 },
		"version-future":     func(value *DocRangeGeneration) { value.Version = 3 },
		"algorithm-zero":     func(value *DocRangeGeneration) { value.Algorithm = 0 },
		"algorithm-future":   func(value *DocRangeGeneration) { value.Algorithm = 2 },
		"document-empty":     func(value *DocRangeGeneration) { value.Hash = "" },
		"document-malformed": func(value *DocRangeGeneration) { value.Hash = "sha256:bad" },
		"envelope-empty":     func(value *DocRangeGeneration) { value.Envelope = nil },
		"envelope-null":      func(value *DocRangeGeneration) { value.Envelope = json.RawMessage(`null`) },
		"envelope-version":   func(value *DocRangeGeneration) { value.Envelope = json.RawMessage(`{"cir_version":"99"}`) },
		"envelope-noncanonical": func(value *DocRangeGeneration) {
			value.Envelope = append([]byte(" "), value.Envelope...)
		},
		"envelope-limit": func(value *DocRangeGeneration) {
			value.Envelope = docRangeGenerationJSON(test, CIREnvelope{CIRVersion: "1", Cwd: strings.Repeat("x", MaxConversationManifestBytes)})
		},
		"negative-stream": func(value *DocRangeGeneration) { value.StreamBytes = -1 },
		"stream-limit": func(value *DocRangeGeneration) {
			value.StreamBytes = int64(MaxConversationDocumentBytes-conversationFrameBytes-len(value.Envelope)) + 1
		},
		"stream-overflow": func(value *DocRangeGeneration) { value.StreamBytes = math.MaxInt64 },
		"negative-events": func(value *DocRangeGeneration) { value.EventCount = -1 },
		"too-many-events": func(value *DocRangeGeneration) { value.EventCount = value.StreamBytes + 1 },
		"event-overflow":  func(value *DocRangeGeneration) { value.EventCount = math.MaxInt64 },
		"no-events":       func(value *DocRangeGeneration) { value.EventCount = 0 },
		"no-stream":       func(value *DocRangeGeneration) { value.StreamBytes = 0 },
		"event-count":     func(value *DocRangeGeneration) { value.EventPages.Count++ },
		"negative-pages":  func(value *DocRangeGeneration) { value.EventPages.Count = -1 },
		"page-overflow":   func(value *DocRangeGeneration) { value.EventPages.Count = math.MaxInt },
		"event-root":      func(value *DocRangeGeneration) { value.EventPages.Hash = "" },
		"event-root-bad":  func(value *DocRangeGeneration) { value.EventPages.Hash = "invalid" },
		"chunk-count":     func(value *DocRangeGeneration) { value.ChunkPages.Count++ },
		"chunk-negative":  func(value *DocRangeGeneration) { value.ChunkPages.Count = -1 },
		"chunk-overflow":  func(value *DocRangeGeneration) { value.ChunkPages.Count = math.MaxInt },
		"chunk-root":      func(value *DocRangeGeneration) { value.ChunkPages.Hash = "" },
		"chunk-root-bad":  func(value *DocRangeGeneration) { value.ChunkPages.Hash = "invalid" },
	} {
		test.Run(name, func(test *testing.T) {
			candidate := valid
			candidate.Envelope = bytes.Clone(valid.Envelope)
			mutate(&candidate)
			if err := candidate.Validate(candidate.Hash); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("invalid claim validated: %v", err)
			}
			if _, err := candidate.Encode(); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("invalid claim encoded: %v", err)
			}
			raw, err := json.Marshal(candidate)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := DecodeDocRangeGeneration(candidate.Hash, HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("invalid claim decoded: %v", err)
			}
		})
	}
	if err := valid.Validate(otherHash); !errors.Is(err, ErrIntegrity) {
		test.Fatal("descriptor not bound to requested document", err)
	}
	empty, _ := docRangeGenerationBuild(test, docRangeGenerationVerified(test, 0))
	for _, root := range []*DocRangeDirectoryRoot{&empty.EventPages, &empty.ChunkPages} {
		root.Hash = otherHash
		if err := empty.Validate(empty.Hash); !errors.Is(err, ErrIntegrity) {
			test.Fatal("empty directory accepted a root hash", err)
		}
		root.Hash = ""
	}
}

func TestDocRangeGenerationDescriptorCanonicalBindingAndLimits(test *testing.T) {
	generation, _ := docRangeGenerationBuild(test, docRangeGenerationVerified(test, 1))
	raw, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	expected := HashContent(raw)
	otherHash := HashContent([]byte("not the bound descriptor or document"))
	for name, candidate := range map[string][]byte{
		"empty":          nil,
		"oversized":      bytes.Repeat([]byte{' '}, MaxDocRangeGenerationBytes+1),
		"leading-space":  append([]byte(" "), raw...),
		"trailing-space": append(bytes.Clone(raw), '\n'),
		"trailing-json":  append(bytes.Clone(raw), []byte(`{}`)...),
		"unknown":        append([]byte(`{"unknown":0,`), raw[1:]...),
		"duplicate":      append([]byte(`{"algorithm":1,`), raw[1:]...),
		"case-alias":     bytes.Replace(raw, []byte(`"algorithm"`), []byte(`"Algorithm"`), 1),
		"missing":        bytes.Replace(raw, []byte(`"algorithm":1,`), nil, 1),
		"null":           bytes.Replace(raw, []byte(`"algorithm":1`), []byte(`"algorithm":null`), 1),
		"float":          bytes.Replace(raw, []byte(`"algorithm":1`), []byte(`"algorithm":1.0`), 1),
		"integer-overflow": bytes.Replace(raw, []byte(`"event_count":1`),
			[]byte(`"event_count":9223372036854775808`), 1),
		"reordered":        append([]byte(`{"version":2,`), bytes.Replace(raw[1:], []byte(`,"version":2`), nil, 1)...),
		"alternate-escape": bytes.Replace(raw, []byte(`"algorithm"`), []byte(`"\u0061lgorithm"`), 1),
		"truncated":        bytes.Clone(raw[:len(raw)-1]),
	} {
		test.Run(name, func(test *testing.T) {
			if bytes.Equal(candidate, raw) {
				test.Fatal("wire mutation did not change fixture")
			}
			decoded, err := DecodeDocRangeGeneration(generation.Hash, HashContent(candidate), candidate)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocRangeGeneration{}) {
				test.Fatalf("invalid wire accepted with recomputed hash: %v", err)
			}
		})
	}
	for _, binding := range [][2]ContentHash{{otherHash, expected}, {generation.Hash, otherHash}, {generation.Hash, ""}, {generation.Hash, "invalid"}} {
		if _, err := DecodeDocRangeGeneration(binding[0], binding[1], raw); !errors.Is(err, ErrIntegrity) {
			test.Fatal("descriptor binding mismatch accepted", err)
		}
	}
	tampered := bytes.Replace(raw, []byte(`"version":2`), []byte(`"version":3`), 1)
	if _, err := DecodeDocRangeGeneration(generation.Hash, expected, tampered); !errors.Is(err, ErrIntegrity) {
		test.Fatal("descriptor tampering accepted", err)
	}
	envelope := CIREnvelope{CIRVersion: "1"}
	envelope.Cwd = strings.Repeat("x", MaxConversationManifestBytes-len(docRangeGenerationJSON(test, envelope)))
	generation.Envelope = docRangeGenerationJSON(test, envelope)
	if len(generation.Envelope) != MaxConversationManifestBytes {
		test.Fatalf("envelope boundary fixture has %d bytes", len(generation.Envelope))
	}
	generation.StreamBytes = int64(MaxConversationDocumentBytes - conversationFrameBytes - len(generation.Envelope))
	generation.EventCount = generation.StreamBytes
	generation.EventPages.Count = int((generation.EventCount + DocRangePageEvents - 1) / DocRangePageEvents)
	generation.ChunkPages.Count = (generation.ChunkCount() + DocRangeChunkPageSize - 1) / DocRangeChunkPageSize
	raw, err = generation.Encode()
	if err != nil || len(raw) > MaxDocRangeGenerationBytes {
		test.Fatalf("valid metadata limits rejected: %d bytes, %v", len(raw), err)
	}
	if _, err := DecodeDocRangeGeneration(generation.Hash, HashContent(raw), raw); err != nil {
		test.Fatal("maximum-sized metadata did not round trip", err)
	}
}

func docRangeGenerationPositions(count int) []int {
	if count == 0 {
		return nil
	}
	positions := []int{0, count / 2, count - 1}
	for _, position := range []int{1, 127, 128, 129, 16383, 16384, 16385} {
		if position < count {
			positions = append(positions, position)
		}
	}
	random := rand.New(rand.NewSource(int64(count)))
	for range 24 {
		positions = append(positions, random.Intn(count))
	}
	slices.Sort(positions)
	return slices.Compact(positions)
}

func docRangeGenerationDepth(count int) int {
	depth := 0
	for count > 0 {
		depth++
		count = (count + DocRangeDirectoryFanout - 1) / DocRangeDirectoryFanout
		if count == 1 {
			break
		}
	}
	return depth
}

func TestDocRangeGenerationDirectoryFanoutAndSelectedPaths(test *testing.T) {
	for _, count := range []int{0, 1, 128, 129, 16384, 16385, 10 * 16385} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			hashes := make([]ContentHash, count)
			for position := range hashes {
				hashes[position] = HashContent([]byte(fmt.Sprintf("leaf/%d", position)))
			}
			var objects []DocRangeObject
			root, err := buildDocRangeDirectory(context.Background(), hashes, &objects)
			if err != nil || root.Count != count || root.validate(count) != nil {
				test.Fatalf("directory build: %+v, %v", root, err)
			}
			wantObjects := 0
			for remaining := count; remaining > 0; {
				remaining = (remaining + DocRangeDirectoryFanout - 1) / DocRangeDirectoryFanout
				wantObjects += remaining
				if remaining == 1 {
					break
				}
			}
			if len(objects) != wantObjects {
				test.Fatalf("built %d directory objects, want %d", len(objects), wantObjects)
			}
			bodies := docRangeGenerationObjects(test, objects)
			if count == 0 {
				if root.Hash != "" {
					test.Fatal("empty directory has a hash")
				}
				if _, err := ResolveDocRangeLeaf(context.Background(), root, 0, func(context.Context, ContentHash) ([]byte, error) {
					test.Fatal("empty directory called loader")
					return nil, nil
				}); !errors.Is(err, ErrIntegrity) {
					test.Fatal("empty directory resolved a leaf", err)
				}
				return
			}
			level, span := docRangeDirectoryLevel(count)
			if level+1 != docRangeGenerationDepth(count) || span < count || (level > 0 && span/DocRangeDirectoryFanout >= count) {
				test.Fatalf("wrong directory height: level=%d span=%d", level, span)
			}
			for _, position := range docRangeGenerationPositions(count) {
				var loaded []ContentHash
				selected, err := ResolveDocRangeLeaf(context.Background(), root, position, func(ctx context.Context, hash ContentHash) ([]byte, error) {
					loaded = append(loaded, hash)
					return docRangeGenerationLoad(bodies)(ctx, hash)
				})
				if err != nil || selected != hashes[position] || len(loaded) != level+1 || loaded[0] != root.Hash {
					test.Fatalf("position %d: got %s, %d loads, %v; want %s, %d loads", position, selected, len(loaded), err, hashes[position], level+1)
				}
				for depth, hash := range loaded {
					var directory docRangeDirectory
					if err := json.Unmarshal(bodies[hash], &directory); err != nil {
						test.Fatal(err)
					}
					childSpan := 1
					for range level - depth {
						childSpan *= DocRangeDirectoryFanout
					}
					start := position / (childSpan * DocRangeDirectoryFanout) * (childSpan * DocRangeDirectoryFanout)
					if directory.Level != level-depth || directory.Start != start || len(directory.Hashes) != min(DocRangeDirectoryFanout, (count-start+childSpan-1)/childSpan) {
						test.Fatalf("position %d depth %d loaded wrong directory: %+v", position, depth, directory)
					}
					next := selected
					if depth+1 < len(loaded) {
						next = loaded[depth+1]
					}
					if directory.Hashes[(position-start)/childSpan] != next {
						test.Fatal("resolver loaded an off-path sibling")
					}
				}
			}
			if count <= 16385 {
				var repeated []DocRangeObject
				again, err := buildDocRangeDirectory(context.Background(), hashes, &repeated)
				if err != nil || again != root || !reflect.DeepEqual(objects, repeated) {
					test.Fatal("directory construction is nondeterministic", err)
				}
			}
		})
	}
}

func docRangeGenerationSparsePath(test testing.TB, count, position int) (DocRangeDirectoryRoot, map[ContentHash][]byte, []ContentHash, ContentHash) {
	test.Helper()
	bodies := make(map[ContentHash][]byte)
	leaf := HashContent([]byte(fmt.Sprintf("selected-leaf/%d", position)))
	selected := leaf
	var path []ContentHash
	for level, childSpan := 0, 1; ; level, childSpan = level+1, childSpan*DocRangeDirectoryFanout {
		span := childSpan * DocRangeDirectoryFanout
		start := position / span * span
		directory := docRangeDirectory{Level: level, Start: start, Hashes: make([]ContentHash, min(DocRangeDirectoryFanout, (count-start+childSpan-1)/childSpan))}
		for offset := range directory.Hashes {
			directory.Hashes[offset] = HashContent([]byte(fmt.Sprintf("unloaded/%d/%d", level, start+offset*childSpan)))
		}
		directory.Hashes[(position-start)/childSpan] = selected
		raw := docRangeGenerationJSON(test, directory)
		selected = HashContent(raw)
		bodies[selected] = raw
		path = append(path, selected)
		if span >= count {
			break
		}
	}
	slices.Reverse(path)
	return DocRangeDirectoryRoot{Hash: selected, Count: count}, bodies, path, leaf
}

func TestDocRangeGenerationDirectoryMaximumSparseSelectedPaths(test *testing.T) {
	for _, count := range []int{128 * 128 * 128, 128*128*128 + 1, MaxDocRangePages} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			for _, position := range docRangeGenerationPositions(count) {
				root, bodies, path, expected := docRangeGenerationSparsePath(test, count, position)
				var loaded []ContentHash
				selected, err := ResolveDocRangeLeaf(context.Background(), root, position, func(ctx context.Context, hash ContentHash) ([]byte, error) {
					loaded = append(loaded, hash)
					return docRangeGenerationLoad(bodies)(ctx, hash)
				})
				if err != nil || selected != expected || !slices.Equal(loaded, path) || len(loaded) != docRangeGenerationDepth(count) {
					test.Fatalf("position %d: selected=%s loads=%d, %v; want %s and path %v", position, selected, len(loaded), err, expected, path)
				}
			}
		})
	}
}

func docRangeGenerationRewritePath(test testing.TB, root DocRangeDirectoryRoot, bodies map[ContentHash][]byte, path []ContentHash, depth int, mutate func([]byte) []byte) (DocRangeDirectoryRoot, map[ContentHash][]byte) {
	test.Helper()
	changed := make(map[ContentHash][]byte, len(bodies))
	for hash, raw := range bodies {
		changed[hash] = raw
	}
	raw := mutate(bytes.Clone(bodies[path[depth]]))
	replacement := HashContent(raw)
	changed[replacement] = raw
	for parent := depth - 1; parent >= 0; parent-- {
		var directory docRangeDirectory
		if err := json.Unmarshal(bodies[path[parent]], &directory); err != nil {
			test.Fatal(err)
		}
		slot := slices.Index(directory.Hashes, path[parent+1])
		if slot < 0 {
			test.Fatal("fixture parent does not reference selected child")
		}
		directory.Hashes[slot] = replacement
		raw = docRangeGenerationJSON(test, directory)
		replacement = HashContent(raw)
		changed[replacement] = raw
	}
	root.Hash = replacement
	return root, changed
}

func TestDocRangeGenerationDirectoryRejectsAuthenticatedMalformedNodes(test *testing.T) {
	root, bodies, path, _ := docRangeGenerationSparsePath(test, 16385, 129)
	mutations := map[string]func(*docRangeDirectory){
		"wrong-start":    func(value *docRangeDirectory) { value.Start++ },
		"negative-start": func(value *docRangeDirectory) { value.Start = -1 },
		"start-overflow": func(value *docRangeDirectory) { value.Start = math.MaxInt },
		"wrong-level":    func(value *docRangeDirectory) { value.Level++ },
		"negative-level": func(value *docRangeDirectory) { value.Level = -1 },
		"level-overflow": func(value *docRangeDirectory) { value.Level = math.MaxInt },
		"short-count":    func(value *docRangeDirectory) { value.Hashes = value.Hashes[:len(value.Hashes)-1] },
		"long-count":     func(value *docRangeDirectory) { value.Hashes = append(value.Hashes, value.Hashes[0]) },
		"empty-count":    func(value *docRangeDirectory) { value.Hashes = []ContentHash{} },
		"null-count":     func(value *docRangeDirectory) { value.Hashes = nil },
		"invalid-first":  func(value *docRangeDirectory) { value.Hashes[0] = "not-a-content-hash" },
		"invalid-last":   func(value *docRangeDirectory) { value.Hashes[len(value.Hashes)-1] = "" },
	}
	for depth := range path {
		for name, mutate := range mutations {
			test.Run(fmt.Sprintf("depth-%d/%s", depth, name), func(test *testing.T) {
				changedRoot, changedBodies := docRangeGenerationRewritePath(test, root, bodies, path, depth, func(raw []byte) []byte {
					var directory docRangeDirectory
					if err := json.Unmarshal(raw, &directory); err != nil {
						test.Fatal(err)
					}
					mutate(&directory)
					return docRangeGenerationJSON(test, directory)
				})
				loads := 0
				selected, err := ResolveDocRangeLeaf(context.Background(), changedRoot, 129, func(ctx context.Context, hash ContentHash) ([]byte, error) {
					loads++
					return docRangeGenerationLoad(changedBodies)(ctx, hash)
				})
				if !errors.Is(err, ErrIntegrity) || selected != "" || loads != depth+1 {
					test.Fatalf("malformed authenticated directory accepted or read descendants: selected=%s loads=%d err=%v", selected, loads, err)
				}
			})
		}
		for name, mutate := range map[string]func([]byte) []byte{
			"empty":          func([]byte) []byte { return nil },
			"huge":           func([]byte) []byte { return bytes.Repeat([]byte{' '}, MaxDocRangeDirectoryBytes+1) },
			"malformed-json": func([]byte) []byte { return []byte(`{`) },
			"whitespace":     func(raw []byte) []byte { return append(raw, '\n') },
			"unknown":        func(raw []byte) []byte { return append([]byte(`{"extra":0,`), raw[1:]...) },
			"duplicate":      func(raw []byte) []byte { return append([]byte(`{"start":0,`), raw[1:]...) },
			"case-alias":     func(raw []byte) []byte { return bytes.Replace(raw, []byte(`"level"`), []byte(`"Level"`), 1) },
			"count-overflow": func(raw []byte) []byte {
				return bytes.Replace(raw, []byte(`"start":`), []byte(`"start":9223372036854775808,"old_start":`), 1)
			},
		} {
			test.Run(fmt.Sprintf("depth-%d/%s", depth, name), func(test *testing.T) {
				changedRoot, changedBodies := docRangeGenerationRewritePath(test, root, bodies, path, depth, mutate)
				selected, err := ResolveDocRangeLeaf(context.Background(), changedRoot, 129, docRangeGenerationLoad(changedBodies))
				if !errors.Is(err, ErrIntegrity) || selected != "" {
					test.Fatalf("invalid authenticated wire accepted: %s, %v", selected, err)
				}
			})
		}
	}
}

func TestDocRangeGenerationDirectoryRejectsInvalidRootsBeforeLoading(test *testing.T) {
	hash := HashContent([]byte("directory"))
	for name, candidate := range map[string]struct {
		root     DocRangeDirectoryRoot
		position int
	}{
		"empty":           {DocRangeDirectoryRoot{}, 0},
		"empty-with-hash": {DocRangeDirectoryRoot{Hash: hash}, 0},
		"missing-hash":    {DocRangeDirectoryRoot{Count: 1}, 0},
		"bad-hash":        {DocRangeDirectoryRoot{Hash: "invalid", Count: 1}, 0},
		"negative-count":  {DocRangeDirectoryRoot{Hash: hash, Count: -1}, 0},
		"count-limit":     {DocRangeDirectoryRoot{Hash: hash, Count: MaxDocRangePages + 1}, 0},
		"count-overflow":  {DocRangeDirectoryRoot{Hash: hash, Count: math.MaxInt}, 0},
		"negative-index":  {DocRangeDirectoryRoot{Hash: hash, Count: 1}, -1},
		"past-end":        {DocRangeDirectoryRoot{Hash: hash, Count: 1}, 1},
		"huge-index":      {DocRangeDirectoryRoot{Hash: hash, Count: 1}, math.MaxInt},
	} {
		test.Run(name, func(test *testing.T) {
			selected, err := ResolveDocRangeLeaf(context.Background(), candidate.root, candidate.position, func(context.Context, ContentHash) ([]byte, error) {
				test.Fatal("invalid root or position reached loader")
				return nil, nil
			})
			if !errors.Is(err, ErrIntegrity) || selected != "" {
				test.Fatalf("invalid root accepted: %s, %v", selected, err)
			}
		})
	}
	if _, err := ResolveDocRangeLeaf(context.Background(), DocRangeDirectoryRoot{Hash: hash, Count: 1}, 0, nil); !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil loader accepted", err)
	}
}

func TestDocRangeGenerationDirectoryLoadFailuresAndCancellation(test *testing.T) {
	root, bodies, path, _ := docRangeGenerationSparsePath(test, 16385, 129)
	failure := errors.New("directory read failed")
	for depth := range path {
		for _, mode := range []string{"missing", "read-error", "wrong-body", "tamper", "cancel"} {
			test.Run(fmt.Sprintf("depth-%d/%s", depth, mode), func(test *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := error(ErrIntegrity)
				loads := 0
				_, err := ResolveDocRangeLeaf(ctx, root, 129, func(received context.Context, hash ContentHash) ([]byte, error) {
					if received != ctx || hash != path[loads] {
						test.Fatal("resolver changed context or loaded wrong path")
					}
					loads++
					if loads != depth+1 {
						return bodies[hash], nil
					}
					switch mode {
					case "missing":
						want = ErrNotFound
						return nil, ErrNotFound
					case "read-error":
						want = failure
						return nil, failure
					case "wrong-body":
						return bodies[path[(depth+1)%len(path)]], nil
					case "tamper":
						return append(bytes.Clone(bodies[hash]), ' '), nil
					case "cancel":
						want = context.Canceled
						cancel()
						return bodies[hash], nil
					}
					panic("unreachable")
				})
				if !errors.Is(err, want) || loads != depth+1 {
					test.Fatalf("got %v after %d loads, want %v after %d", err, loads, want, depth+1)
				}
			})
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ResolveDocRangeLeaf(ctx, root, 129, func(context.Context, ContentHash) ([]byte, error) {
		test.Fatal("pre-canceled resolver loaded data")
		return nil, nil
	}); !errors.Is(err, context.Canceled) {
		test.Fatal("pre-cancellation was ignored", err)
	}
	expired, expire := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer expire()
	if _, err := ResolveDocRangeLeaf(expired, root, 129, nil); !errors.Is(err, context.DeadlineExceeded) {
		test.Fatal("expired deadline was not preserved", err)
	}
}

func TestDocRangeGenerationEventPageDecoderParity(test *testing.T) {
	verified := docRangeGenerationVerified(test, 2*DocRangePageEvents+3)
	header := verified.Header()
	generation, objects := docRangeGenerationBuild(test, verified)
	bodies := docRangeGenerationObjects(test, objects)
	for position := range header.Pages {
		test.Run(fmt.Sprint(position), func(test *testing.T) {
			raw := docRangeTestPage(test, verified, position)
			selected, err := ResolveDocRangeLeaf(context.Background(), generation.EventPages, position, docRangeGenerationLoad(bodies))
			if err != nil {
				test.Fatal(err)
			}
			entries, err := generation.DecodePage(position, selected, raw)
			if err != nil {
				test.Fatal(err)
			}
			for name, mutate := range map[string]func([]DocRangeEntry) []DocRangeEntry{
				"short":           func(value []DocRangeEntry) []DocRangeEntry { return value[:len(value)-1] },
				"long":            func(value []DocRangeEntry) []DocRangeEntry { return append(value, value[len(value)-1]) },
				"index":           func(value []DocRangeEntry) []DocRangeEntry { value[0].Index++; return value },
				"negative-offset": func(value []DocRangeEntry) []DocRangeEntry { value[0].Offset = -1; return value },
				"negative-length": func(value []DocRangeEntry) []DocRangeEntry { value[0].Length = -1; return value },
				"zero-length":     func(value []DocRangeEntry) []DocRangeEntry { value[0].Length = 0; return value },
				"past-stream": func(value []DocRangeEntry) []DocRangeEntry {
					value[len(value)-1].Offset = int(generation.StreamBytes)
					return value
				},
				"offset-overflow": func(value []DocRangeEntry) []DocRangeEntry { value[0].Offset = math.MaxInt; return value },
				"length-overflow": func(value []DocRangeEntry) []DocRangeEntry { value[0].Length = math.MaxInt; return value },
				"gap":             func(value []DocRangeEntry) []DocRangeEntry { value[1].Offset++; return value },
				"overlap":         func(value []DocRangeEntry) []DocRangeEntry { value[1].Offset--; return value },
				"invalid-hash":    func(value []DocRangeEntry) []DocRangeEntry { value[0].Hash = "bad"; return value },
			} {
				test.Run(name, func(test *testing.T) {
					candidate := docRangeGenerationJSON(test, mutate(slices.Clone(entries)))
					changedHeader := header
					changedHeader.Pages = slices.Clone(header.Pages)
					changedHeader.Pages[position] = HashContent(candidate)
					oldEntries, oldErr := changedHeader.DecodePage(position, candidate)
					newEntries, newErr := generation.DecodePage(position, HashContent(candidate), candidate)
					if !errors.Is(oldErr, ErrIntegrity) || !errors.Is(newErr, ErrIntegrity) || oldEntries != nil || newEntries != nil {
						test.Fatalf("decoder parity lost: old=%v new=%v", oldErr, newErr)
					}
				})
			}
			for name, candidate := range map[string][]byte{
				"nil": nil, "null": []byte(`null`), "empty": []byte(`[]`), "invalid-json": []byte(`[`),
				"huge": bytes.Repeat([]byte{' '}, MaxDocRangePageBytes+1),
			} {
				test.Run(name, func(test *testing.T) {
					changedHeader := header
					changedHeader.Pages = slices.Clone(header.Pages)
					changedHeader.Pages[position] = HashContent(candidate)
					_, oldErr := changedHeader.DecodePage(position, candidate)
					_, newErr := generation.DecodePage(position, HashContent(candidate), candidate)
					if !errors.Is(oldErr, ErrIntegrity) || !errors.Is(newErr, ErrIntegrity) {
						test.Fatalf("wire parity lost: old=%v new=%v", oldErr, newErr)
					}
				})
			}
			wrongPage := docRangeTestPage(test, verified, (position+1)%len(header.Pages))
			if _, err := generation.DecodePage(position, selected, wrongPage); !errors.Is(err, ErrIntegrity) {
				test.Fatal("selected leaf did not bind page bytes", err)
			}
			if _, err := generation.DecodePage(position, HashContent([]byte("wrong expected leaf")), raw); !errors.Is(err, ErrIntegrity) {
				test.Fatal("wrong selected leaf accepted", err)
			}
			before := slices.Clone(entries)
			for offset := range raw {
				raw[offset] = 'x'
			}
			if !reflect.DeepEqual(entries, before) {
				test.Fatal("decoded event entries alias source bytes")
			}
		})
	}
	for _, position := range []int{-1, len(header.Pages), math.MaxInt} {
		raw := docRangeTestPage(test, verified, 0)
		_, oldErr := header.DecodePage(position, raw)
		_, newErr := generation.DecodePage(position, header.Pages[0], raw)
		if !errors.Is(oldErr, ErrIntegrity) || !errors.Is(newErr, ErrIntegrity) {
			test.Fatalf("position %d parity: old=%v new=%v", position, oldErr, newErr)
		}
	}
}

func TestDocRangeGenerationChunkDescriptorPartitions(test *testing.T) {
	base, _ := docRangeGenerationBuild(test, docRangeGenerationVerified(test, 1))
	for _, count := range []int{0, 1, 127, 128, 129, 256, 257, MaxConversationDocumentBytes / ConversationManifestChunkBytes} {
		for _, partial := range []bool{false, true} {
			test.Run(fmt.Sprintf("count-%d/partial-%t", count, partial), func(test *testing.T) {
				generation := base
				chunks := make([]ConversationManifestChunk, count)
				for ordinal := range chunks {
					chunks[ordinal] = ConversationManifestChunk{Hash: HashContent([]byte(fmt.Sprintf("chunk/%d", ordinal))), Bytes: ConversationManifestChunkBytes}
				}
				if count > 0 && partial {
					chunks[count-1].Bytes = 17
				}
				if count == MaxConversationDocumentBytes/ConversationManifestChunkBytes && !partial {
					chunks[count-1].Bytes -= int64(conversationFrameBytes + len(generation.Envelope))
				}
				generation.StreamBytes = 0
				for _, chunk := range chunks {
					generation.StreamBytes += chunk.Bytes
				}
				if count == 0 {
					generation.EventCount = 0
					generation.EventPages = DocRangeDirectoryRoot{}
				}
				var pages []ContentHash
				var objects []DocRangeObject
				for start := 0; start < count; start += DocRangeChunkPageSize {
					raw := docRangeGenerationJSON(test, chunks[start:min(start+DocRangeChunkPageSize, count)])
					hash := HashContent(raw)
					pages = append(pages, hash)
					objects = append(objects, DocRangeObject{Hash: hash, Bytes: raw})
				}
				root, err := buildDocRangeDirectory(context.Background(), pages, &objects)
				if err != nil {
					test.Fatal(err)
				}
				generation.ChunkPages = root
				if generation.ChunkCount() != count || generation.Validate(generation.Hash) != nil {
					test.Fatal("synthetic descriptor partition is not structurally valid")
				}
				bodies := docRangeGenerationObjects(test, objects)
				decoded := make([]ConversationManifestChunk, 0, count)
				for position, expected := range pages {
					selected, err := ResolveDocRangeLeaf(context.Background(), root, position, docRangeGenerationLoad(bodies))
					if err != nil || selected != expected {
						test.Fatal("wrong chunk descriptor page selected", err)
					}
					page, err := generation.DecodeChunks(position, selected, bodies[selected])
					if err != nil || len(page) != min(DocRangeChunkPageSize, count-position*DocRangeChunkPageSize) {
						test.Fatalf("page %d failed partition: %v", position, err)
					}
					decoded = append(decoded, page...)
				}
				if !slices.Equal(decoded, chunks) {
					test.Fatal("chunk partition changed descriptor order, hash, or lengths")
				}
				for _, position := range []int{-1, len(pages), math.MaxInt} {
					if _, err := generation.DecodeChunks(position, HashContent([]byte(`[]`)), []byte(`[]`)); !errors.Is(err, ErrIntegrity) {
						test.Fatalf("invalid chunk page position %d accepted: %v", position, err)
					}
				}
			})
		}
	}
}

func TestDocRangeGenerationChunkDecoderRejectsTamperingAndBadPartitions(test *testing.T) {
	generation, _ := docRangeGenerationBuild(test, docRangeGenerationVerified(test, 1))
	generation.StreamBytes = DocRangeChunkPageSize*ConversationManifestChunkBytes + 17
	generation.ChunkPages.Count = 2
	for _, position := range []int{0, 1} {
		chunks := make([]ConversationManifestChunk, min(DocRangeChunkPageSize, generation.ChunkCount()-position*DocRangeChunkPageSize))
		for offset := range chunks {
			chunks[offset] = ConversationManifestChunk{Hash: HashContent([]byte(fmt.Sprintf("chunk/%d/%d", position, offset))), Bytes: ConversationManifestChunkBytes}
			if position == 1 {
				chunks[offset].Bytes = 17
			}
		}
		for name, mutate := range map[string]func([]ConversationManifestChunk) []ConversationManifestChunk{
			"missing": func(value []ConversationManifestChunk) []ConversationManifestChunk { return value[:len(value)-1] },
			"extra":   func(value []ConversationManifestChunk) []ConversationManifestChunk { return append(value, value[0]) },
			"hash": func(value []ConversationManifestChunk) []ConversationManifestChunk {
				value[0].Hash = "bad"
				return value
			},
			"zero": func(value []ConversationManifestChunk) []ConversationManifestChunk { value[0].Bytes = 0; return value },
			"negative": func(value []ConversationManifestChunk) []ConversationManifestChunk {
				value[0].Bytes = -1
				return value
			},
			"short": func(value []ConversationManifestChunk) []ConversationManifestChunk { value[0].Bytes--; return value },
			"long":  func(value []ConversationManifestChunk) []ConversationManifestChunk { value[0].Bytes++; return value },
			"overflow": func(value []ConversationManifestChunk) []ConversationManifestChunk {
				value[0].Bytes = math.MaxInt64
				return value
			},
		} {
			test.Run(fmt.Sprintf("page-%d/%s", position, name), func(test *testing.T) {
				raw := docRangeGenerationJSON(test, mutate(slices.Clone(chunks)))
				if _, err := generation.DecodeChunks(position, HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
					test.Fatal("invalid authenticated chunk partition accepted", err)
				}
			})
		}
		raw := docRangeGenerationJSON(test, chunks)
		expected := HashContent(raw)
		for name, candidate := range map[string][]byte{
			"empty": nil, "null": []byte(`null`), "malformed": []byte(`[`),
			"huge": bytes.Repeat([]byte{' '}, MaxDocRangeDirectoryBytes+1),
		} {
			test.Run(fmt.Sprintf("page-%d/%s", position, name), func(test *testing.T) {
				if _, err := generation.DecodeChunks(position, HashContent(candidate), candidate); !errors.Is(err, ErrIntegrity) {
					test.Fatal("invalid authenticated chunk page accepted", err)
				}
			})
		}
		if _, err := generation.DecodeChunks(position, HashContent([]byte("wrong selected leaf")), raw); !errors.Is(err, ErrIntegrity) {
			test.Fatal("unbound chunk page accepted", err)
		}
		if _, err := generation.DecodeChunks(position, expected, append(bytes.Clone(raw), ' ')); !errors.Is(err, ErrIntegrity) {
			test.Fatal("tampered chunk page accepted", err)
		}
		decoded, err := generation.DecodeChunks(position, expected, raw)
		if err != nil {
			test.Fatal(err)
		}
		decoded[0].Hash = "changed"
		again, err := generation.DecodeChunks(position, expected, raw)
		if err != nil || !slices.Equal(again, chunks) {
			test.Fatal("returned chunk descriptor aliases later decode", err)
		}
		for offset := range raw {
			raw[offset] = 'x'
		}
		if !slices.Equal(again, chunks) {
			test.Fatal("decoded chunk descriptors alias source bytes")
		}
	}
}

func TestDocRangeGenerationConcurrentBuildAndSelectedReads(test *testing.T) {
	verified := docRangeGenerationVerified(test, 2*DocRangePageEvents+1)
	want, wantObjects := docRangeGenerationBuild(test, verified)
	bodies := docRangeGenerationObjects(test, wantObjects)
	header := verified.Header()
	var workers sync.WaitGroup
	failures := make(chan error, 8)
	for worker := range 8 {
		workers.Go(func() {
			for iteration := range 4 {
				generation, objects, err := BuildDocRangeGeneration(context.Background(), verified)
				if err != nil || !reflect.DeepEqual(generation, want) || !reflect.DeepEqual(objects, wantObjects) {
					failures <- fmt.Errorf("concurrent build changed: %w", err)
					return
				}
				position := (worker + iteration) % len(header.Pages)
				selected, err := ResolveDocRangeLeaf(context.Background(), generation.EventPages, position, docRangeGenerationLoad(bodies))
				if err != nil || selected != header.Pages[position] {
					failures <- fmt.Errorf("concurrent selected path changed: %w", err)
					return
				}
				generation.Envelope[0] = 'x'
				for offset := range objects {
					objects[offset].Bytes[0] = 'x'
				}
			}
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		test.Error(err)
	}
}
