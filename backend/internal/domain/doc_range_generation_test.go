package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
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
			_, err := json.Marshal(candidate)
			if err != nil {
				test.Fatal(err)
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
				_, _ = docRangeGenerationRewritePath(test, root, bodies, path, depth, func(raw []byte) []byte {
					var directory docRangeDirectory
					if err := json.Unmarshal(raw, &directory); err != nil {
						test.Fatal(err)
					}
					mutate(&directory)
					return docRangeGenerationJSON(test, directory)
				})
				_ = 0
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
				_, _ = docRangeGenerationRewritePath(test, root, bodies, path, depth, mutate)
			})
		}
	}
}

func TestDocRangeGenerationConcurrentBuildAndSelectedReads(test *testing.T) {
	verified := docRangeGenerationVerified(test, 2*DocRangePageEvents+1)
	want, wantObjects := docRangeGenerationBuild(test, verified)
	_ = docRangeGenerationObjects(test, wantObjects)
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
				_ = (worker + iteration) % len(header.Pages)
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
