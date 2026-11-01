package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unsafe"
)

func legacyDerivedMetadataFixture(test testing.TB, count int) (VerifiedLegacyDerivedRangeMetadata, []DocRangeObject) {
	test.Helper()
	proof, _ := legacyDocRangeGenerationFixture(test, count)
	var saved []DocRangeObject
	metadata, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(ctx context.Context, object DocRangeObject) error {
		saved = append(saved, object)
		return ctx.Err()
	})
	if err != nil || !metadata.Valid() || metadata.ObjectCount() != len(saved) {
		test.Fatalf("prepare metadata fixture: count=%d saved=%d error=%v", metadata.ObjectCount(), len(saved), err)
	}
	return metadata, saved
}

func TestLegacyDerivedRangeMetadataOutputParity(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		build func(testing.TB) (VerifiedSessionDoc, []byte)
	}{
		{name: "empty", build: func(test testing.TB) (VerifiedSessionDoc, []byte) { return legacyDocRangeGenerationFixture(test, 0) }},
		{name: "mixed-events", build: func(test testing.TB) (VerifiedSessionDoc, []byte) { return legacyDocRangeGenerationFixture(test, 9) }},
		{name: "event-pages", build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocRangeGenerationFixture(test, 2*DocRangePageEvents+7)
		}},
		{name: "exact-byte-block", build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocByteBackfillSizedFixture(test, ConversationManifestChunkBytes)
		}},
		{name: "three-byte-blocks", build: legacyDocByteGenerationLargeFixture},
		{name: "split-utf8", build: func(test testing.TB) (VerifiedSessionDoc, []byte) {
			return legacyDocByteBackfillBoundaryFixture(test, "한🙂")
		}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			proof, original := fixture.build(test)
			want, objects, err := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
			if err != nil {
				test.Fatal(err)
			}
			plan, err := NewLegacyByteBackfill(context.Background(), proof)
			if err != nil {
				test.Fatal(err)
			}
			key, err := plan.Key()
			if err != nil {
				test.Fatal(err)
			}
			var saved []DocRangeObject
			metadata, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(ctx context.Context, object DocRangeObject) error {
				if len(object.Bytes) == 0 || len(object.Bytes) > MaxDocRangePageBytes || HashContent(object.Bytes) != object.Hash {
					test.Fatal("preparation saved an unbounded or unauthenticated object")
				}
				saved = append(saved, object)
				return ctx.Err()
			})
			if err != nil || !metadata.Valid() || metadata.Reference() != proof.DocumentRef() || metadata.PlanKey() != key ||
				metadata.ObjectCount() != len(objects) || len(saved) != len(objects) {
				test.Fatalf("metadata differs from the source and backfill plan: %v", err)
			}
			got, err := metadata.Descriptor()
			if err != nil || !reflect.DeepEqual(got, want) || got.ByteStorage() != DocRangeByteStorageDerived {
				test.Fatalf("metadata changed the generation descriptor: %+v, %v", got, err)
			}
			for ordinal, expected := range objects {
				if saved[ordinal].Hash != expected.Hash || !bytes.Equal(saved[ordinal].Bytes, expected.Bytes) {
					test.Fatalf("saved metadata object %d differs from the builder", ordinal)
				}
				object, err := metadata.Object(context.Background(), ordinal, func(ctx context.Context, selected int) ([]byte, error) {
					return saved[selected].Bytes, ctx.Err()
				})
				if err != nil || object.Hash != expected.Hash || !bytes.Equal(object.Bytes, expected.Bytes) {
					test.Fatalf("loaded metadata object %d differs from the builder: %v", ordinal, err)
				}
			}
			if !bytes.Equal(proof.Bytes(), original) || proof.DocumentRef() != metadata.Reference() {
				test.Fatal("preparation changed the verified source")
			}
		})
	}
}

func TestLegacyDerivedRangeMetadataLoadsOnlySelectedOrdinals(test *testing.T) {
	metadata, saved := legacyDerivedMetadataFixture(test, 2*DocRangePageEvents+7)
	selected := []int{len(saved) - 1, 0, 1, len(saved) - 1, 0}
	var reads []int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, ordinal := range selected {
		object, err := metadata.Object(ctx, ordinal, func(loadCtx context.Context, position int) ([]byte, error) {
			if loadCtx != ctx {
				test.Fatal("selected load lost its request context")
			}
			reads = append(reads, position)
			return saved[position].Bytes, nil
		})
		if err != nil || object.Hash != saved[ordinal].Hash || !bytes.Equal(object.Bytes, saved[ordinal].Bytes) {
			test.Fatalf("selected ordinal %d changed its object: %v", ordinal, err)
		}
	}
	if !slices.Equal(reads, selected) {
		test.Fatalf("loaded ordinals %v, want exactly %v", reads, selected)
	}
}

func TestLegacyDerivedRangeMetadataSaveMutationCannotChangeExpectations(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, DocRangePageEvents+1)
	_, expected, err := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	var saved []DocRangeObject
	metadata, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(ctx context.Context, object DocRangeObject) error {
		ordinal := len(saved)
		if object.Hash != expected[ordinal].Hash || !bytes.Equal(object.Bytes, expected[ordinal].Bytes) {
			test.Fatal("callback did not receive the original metadata object")
		}
		object.Bytes[0] ^= 1
		object.Hash = HashContent(object.Bytes)
		saved = append(saved, object)
		return ctx.Err()
	})
	if err != nil || !metadata.Valid() {
		test.Fatalf("save callback mutation damaged capsule construction: %v", err)
	}
	for ordinal := range saved {
		object, err := metadata.Object(context.Background(), ordinal, func(context.Context, int) ([]byte, error) {
			return saved[ordinal].Bytes, nil
		})
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(object, DocRangeObject{}) {
			test.Fatalf("save callback rewrote object %d's expected hash: %+v, %v", ordinal, object, err)
		}
		object, err = metadata.Object(context.Background(), ordinal, func(context.Context, int) ([]byte, error) {
			return expected[ordinal].Bytes, nil
		})
		if err != nil || object.Hash != expected[ordinal].Hash || !bytes.Equal(object.Bytes, expected[ordinal].Bytes) {
			test.Fatalf("private expectations followed save callback mutation for object %d: %v", ordinal, err)
		}
	}
}

func TestLegacyDerivedRangeMetadataObjectOwnsLoadSnapshot(test *testing.T) {
	metadata, saved := legacyDerivedMetadataFixture(test, 9)
	for _, corrupted := range []bool{false, true} {
		test.Run(fmt.Sprintf("corrupted-%t", corrupted), func(test *testing.T) {
			raw := bytes.Clone(saved[0].Bytes)
			if corrupted {
				raw[0] ^= 1
			}
			loaded, mutated := false, false
			ctx := canonicalAdmissionContext{Context: context.Background(), check: func() {
				if loaded && !mutated {
					raw[0] ^= 1
					mutated = true
				}
			}}
			object, err := metadata.Object(ctx, 0, func(context.Context, int) ([]byte, error) {
				loaded = true
				return raw, nil
			})
			if !mutated {
				test.Fatal("load snapshot mutation checkpoint was not reached")
			}
			if corrupted {
				if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(object, DocRangeObject{}) {
					test.Fatalf("validation followed mutable callback bytes instead of its clone: %+v, %v", object, err)
				}
				return
			}
			if err != nil || !bytes.Equal(object.Bytes, saved[0].Bytes) || &object.Bytes[0] == &raw[0] {
				test.Fatalf("object did not validate an independently owned load snapshot: %v", err)
			}
			object.Bytes[0] ^= 2
			if bytes.Equal(object.Bytes, raw) || !bytes.Equal(saved[0].Bytes, metadataObjectReload(test, metadata, saved, 0).Bytes) {
				test.Fatal("returned object mutation changed callback storage or later reads")
			}
		})
	}
}

func metadataObjectReload(test testing.TB, metadata VerifiedLegacyDerivedRangeMetadata, saved []DocRangeObject, ordinal int) DocRangeObject {
	test.Helper()
	object, err := metadata.Object(context.Background(), ordinal, func(ctx context.Context, selected int) ([]byte, error) {
		return saved[selected].Bytes, ctx.Err()
	})
	if err != nil {
		test.Fatal(err)
	}
	return object
}

func TestLegacyDerivedRangeMetadataRejectsBadLoadsWithoutData(test *testing.T) {
	metadata, saved := legacyDerivedMetadataFixture(test, DocRangePageEvents+1)
	want := saved[0].Bytes
	corrupted := bytes.Clone(want)
	corrupted[len(corrupted)/2] ^= 1
	loadFailure := errors.New("selected metadata load failed")
	for _, fixture := range []struct {
		name string
		raw  []byte
		err  error
		want error
	}{
		{name: "same-length-corruption", raw: corrupted, want: ErrIntegrity},
		{name: "different-object", raw: saved[len(saved)-1].Bytes, want: ErrIntegrity},
		{name: "truncated", raw: want[:len(want)-1], want: ErrIntegrity},
		{name: "extended", raw: append(bytes.Clone(want), ' '), want: ErrIntegrity},
		{name: "nil", want: ErrIntegrity},
		{name: "empty", raw: []byte{}, want: ErrIntegrity},
		{name: "oversized", raw: make([]byte, MaxDocRangePageBytes+1), want: ErrIntegrity},
		{name: "error-with-bytes", raw: want, err: loadFailure, want: loadFailure},
		{name: "error-without-bytes", err: loadFailure, want: loadFailure},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			reads := 0
			object, err := metadata.Object(context.Background(), 0, func(context.Context, int) ([]byte, error) {
				reads++
				return fixture.raw, fixture.err
			})
			if !errors.Is(err, fixture.want) || !reflect.DeepEqual(object, DocRangeObject{}) || reads != 1 {
				test.Fatalf("bad load exposed an object or changed error/read count: %+v, %v, %d", object, err, reads)
			}
		})
	}
	for _, ordinal := range []int{-1, metadata.ObjectCount(), metadata.ObjectCount() + 1} {
		reads := 0
		object, err := metadata.Object(context.Background(), ordinal, func(context.Context, int) ([]byte, error) {
			reads++
			return want, nil
		})
		if !errors.Is(err, ErrValidation) || !reflect.DeepEqual(object, DocRangeObject{}) || reads != 0 {
			test.Fatalf("invalid ordinal %d reached storage or returned data: %+v, %v", ordinal, object, err)
		}
	}
	if object, err := metadata.Object(context.Background(), 0, nil); !errors.Is(err, ErrValidation) || !reflect.DeepEqual(object, DocRangeObject{}) {
		test.Fatalf("nil loader produced an object: %+v, %v", object, err)
	}
}

func TestLegacyDerivedRangeMetadataDescriptorAndReferenceOwnership(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, 9)
	backing := string(proof.Hash()) + strings.Repeat("unowned-hash-backing", 4096)
	proof.hash = ContentHash(backing[:len(proof.Hash())])
	metadata, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(context.Context, DocRangeObject) error { return nil })
	if err != nil {
		test.Fatal(err)
	}
	reference := metadata.Reference()
	if reference != proof.DocumentRef() || unsafe.StringData(string(reference.Hash)) == unsafe.StringData(string(proof.Hash())) {
		test.Fatal("metadata reference borrows the source proof's string allocation")
	}
	want, err := metadata.Descriptor()
	if err != nil {
		test.Fatal(err)
	}
	changed, err := metadata.Descriptor()
	if err != nil || &changed.Envelope[0] == &want.Envelope[0] || changed.SourceIdentity == want.SourceIdentity ||
		unsafe.StringData(string(changed.Hash)) == unsafe.StringData(string(want.Hash)) {
		test.Fatalf("decoded descriptors share mutable metadata or source strings: %v", err)
	}
	changed.Envelope[0] ^= 1
	*changed.SourceIdentity = DocumentIdentityRootV1
	changed.Hash = HashContent([]byte("changed descriptor"))
	changed.EventPages.Count++
	changed.ChunkPages.Hash = changed.Hash
	reference.Hash = changed.Hash
	reference.Identity = DocumentIdentityRootV1
	again, err := metadata.Descriptor()
	if err != nil || !reflect.DeepEqual(again, want) || metadata.Reference() != proof.DocumentRef() || metadata.PlanKey() == "" {
		test.Fatalf("exported metadata mutation changed the private capsule: %v", err)
	}
}

func TestLegacyDerivedRangeMetadataSerializedDescriptorCannotSeal(test *testing.T) {
	metadata, _ := legacyDerivedMetadataFixture(test, 9)
	descriptor, err := metadata.Descriptor()
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := descriptor.Encode()
	if err != nil {
		test.Fatal(err)
	}
	serialized, err := json.Marshal(metadata)
	if err != nil || string(serialized) != "{}" {
		test.Fatalf("capsule exposed private fields through serialization: %s, %v", serialized, err)
	}
	for _, raw := range [][]byte{encoded, serialized, []byte(`{"sealed":true,"Valid":true,"descriptor":{},"objects":[]}`)} {
		var decoded VerifiedLegacyDerivedRangeMetadata
		if err := json.Unmarshal(raw, &decoded); err != nil {
			test.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, VerifiedLegacyDerivedRangeMetadata{}) || decoded.Valid() {
			test.Fatal("serialized metadata minted a verified capsule")
		}
	}
	unsealed := metadata
	unsealed.sealed = false
	for _, invalid := range []VerifiedLegacyDerivedRangeMetadata{{}, unsealed} {
		if invalid.Valid() || invalid.Reference() != (DocumentRef{}) || invalid.PlanKey() != "" || invalid.ObjectCount() != 0 {
			test.Fatal("unsealed capsule exposed trusted metadata")
		}
		if descriptor, err := invalid.Descriptor(); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(descriptor, DocRangeGeneration{}) {
			test.Fatalf("unsealed capsule exposed a descriptor: %+v, %v", descriptor, err)
		}
		if retained, err := invalid.RetainedBytes(); !errors.Is(err, ErrIntegrity) || retained != 0 {
			test.Fatalf("unsealed capsule exposed a retained-byte estimate: %d, %v", retained, err)
		}
		object, err := invalid.Object(context.Background(), 0, func(context.Context, int) ([]byte, error) {
			test.Fatal("unsealed capsule reached object storage")
			return nil, nil
		})
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(object, DocRangeObject{}) {
			test.Fatalf("unsealed capsule exposed an object: %+v, %v", object, err)
		}
	}
}

func TestLegacyDerivedRangeMetadataRejectsInvalidAndRootSources(test *testing.T) {
	proof, original := legacyDocRangeGenerationFixture(test, 1)
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
	if err != nil {
		test.Fatal(err)
	}
	for name, source := range map[string]VerifiedSessionDoc{
		"zero": {}, "hash-only": {hash: proof.Hash()}, "missing-hash": {canonical: string(original)},
		"unknown-identity": {hash: proof.Hash(), canonical: string(original), identity: "future"}, "root": root,
	} {
		test.Run(name, func(test *testing.T) {
			metadata, err := PrepareLegacyDerivedRangeMetadata(context.Background(), source, func(context.Context, DocRangeObject) error {
				test.Fatal("unsupported source reached save callback")
				return nil
			})
			if !errors.Is(err, ErrUnsupportedDocumentIdentity) || !reflect.DeepEqual(metadata, VerifiedLegacyDerivedRangeMetadata{}) {
				test.Fatalf("unsupported source produced a capsule: %+v, %v", metadata, err)
			}
		})
	}
	if metadata, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, nil); !errors.Is(err, ErrValidation) || !reflect.DeepEqual(metadata, VerifiedLegacyDerivedRangeMetadata{}) {
		test.Fatalf("nonempty source accepted a nil save callback: %+v, %v", metadata, err)
	}
	empty, _ := legacyDocRangeGenerationFixture(test, 0)
	metadata, err := PrepareLegacyDerivedRangeMetadata(context.Background(), empty, nil)
	if err != nil || !metadata.Valid() || metadata.ObjectCount() != 0 || metadata.Reference() != empty.DocumentRef() {
		test.Fatalf("empty source failed without a save callback: %v", err)
	}
	descriptor, err := metadata.Descriptor()
	if err != nil || descriptor.EventCount != 0 || descriptor.StreamBytes != 0 || descriptor.EventPages.Count != 0 || descriptor.ChunkPages.Count != 0 {
		test.Fatalf("empty source acquired byte or event metadata: %+v, %v", descriptor, err)
	}
	if object, err := metadata.Object(context.Background(), 0, nil); !errors.Is(err, ErrValidation) || !reflect.DeepEqual(object, DocRangeObject{}) {
		test.Fatalf("empty source exposed an object: %+v, %v", object, err)
	}
}

func TestLegacyDerivedRangeMetadataSaveFailuresAndCancellation(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, DocRangePageEvents+1)
	baseline, saved := legacyDerivedMetadataFixture(test, DocRangePageEvents+1)
	saveFailure := errors.New("metadata save failed")
	for _, cancelSave := range []bool{false, true} {
		for _, failAt := range []int{0, len(saved) / 2, len(saved) - 1} {
			test.Run(fmt.Sprintf("cancel-%t-ordinal-%d", cancelSave, failAt), func(test *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				metadata, err := PrepareLegacyDerivedRangeMetadata(ctx, proof, func(context.Context, DocRangeObject) error {
					ordinal := calls
					calls++
					if ordinal == failAt {
						if cancelSave {
							cancel()
							return nil
						}
						return saveFailure
					}
					return nil
				})
				want := saveFailure
				if cancelSave {
					want = context.Canceled
				}
				if !errors.Is(err, want) || !reflect.DeepEqual(metadata, VerifiedLegacyDerivedRangeMetadata{}) || calls != failAt+1 {
					test.Fatalf("partial save exposed a capsule or continued saving: calls=%d, %v", calls, err)
				}
			})
		}
	}
	for _, cancelAt := range []int{0, 1, 2} {
		ctx, cancel := context.WithCancel(context.Background())
		loaded, checks := false, 0
		if cancelAt == 0 {
			cancel()
		}
		observed := canonicalAdmissionContext{Context: ctx, check: func() {
			if loaded {
				checks++
				if checks == cancelAt {
					cancel()
				}
			}
		}}
		object, err := baseline.Object(observed, 0, func(context.Context, int) ([]byte, error) {
			loaded = true
			return saved[0].Bytes, nil
		})
		cancel()
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(object, DocRangeObject{}) || cancelAt == 0 && loaded {
			test.Fatalf("canceled selected read exposed data or loaded early: checkpoint=%d, %v", cancelAt, err)
		}
	}
}

func TestLegacyDerivedRangeMetadataPreparationCancellationCheckpoints(test *testing.T) {
	for _, count := range []int{0, 9} {
		proof, _ := legacyDocRangeGenerationFixture(test, count)
		checks := 0
		observed := canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}
		if _, err := PrepareLegacyDerivedRangeMetadata(observed, proof, func(context.Context, DocRangeObject) error { return nil }); err != nil {
			test.Fatal(err)
		}
		for cancelAt := 1; cancelAt <= checks; cancelAt++ {
			ctx, cancel := context.WithCancel(context.Background())
			seen := 0
			observed := canonicalAdmissionContext{Context: ctx, check: func() {
				seen++
				if seen == cancelAt {
					cancel()
				}
			}}
			metadata, err := PrepareLegacyDerivedRangeMetadata(observed, proof, func(context.Context, DocRangeObject) error { return nil })
			cancel()
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(metadata, VerifiedLegacyDerivedRangeMetadata{}) {
				test.Fatalf("preparation checkpoint %d/%d retained a capsule for %d events: %v", cancelAt, checks, count, err)
			}
		}
	}
}

func TestLegacyDerivedRangeMetadataRetainedBytesCoverCapacity(test *testing.T) {
	metadata, _ := legacyDerivedMetadataFixture(test, 9)
	retained, err := metadata.RetainedBytes()
	minimum := int64(unsafe.Sizeof(metadata)) + int64(cap(metadata.descriptor)) +
		int64(cap(metadata.objects))*int64(unsafe.Sizeof(legacyDerivedRangeObjectMetadata{})) +
		int64(len(metadata.descriptorHash)+len(metadata.reference.Hash)+len(metadata.reference.Identity)+len(metadata.planKey))
	for _, object := range metadata.objects[:cap(metadata.objects)] {
		minimum += int64(len(object.hash))
	}
	if err != nil || retained < minimum || retained >= 64<<10 {
		test.Fatalf("metadata accounting is not a small conservative owned-heap estimate: retained=%d minimum=%d error=%v", retained, minimum, err)
	}
	for _, part := range []string{"descriptor-capacity", "object-capacity-and-tail-hash"} {
		grown := metadata
		const extra = 4096
		var increase int64
		switch part {
		case "descriptor-capacity":
			grown.descriptor = make([]byte, len(metadata.descriptor), cap(metadata.descriptor)+extra)
			copy(grown.descriptor, metadata.descriptor)
			increase = extra
		case "object-capacity-and-tail-hash":
			objects := make([]legacyDerivedRangeObjectMetadata, cap(metadata.objects)+1)
			copy(objects, metadata.objects)
			objects[len(objects)-1].hash = ContentHash(strings.Repeat("tail", extra))
			grown.objects = objects[:len(metadata.objects)]
			grown.objectHeap.allocation(int64(len(objects[len(objects)-1].hash)), 1)
			increase = int64(len(objects[len(objects)-1].hash)) + int64(unsafe.Sizeof(legacyDerivedRangeObjectMetadata{}))
		}
		after, err := grown.RetainedBytes()
		if err != nil || after-retained < increase {
			test.Fatalf("%s escaped retained-byte accounting: before=%d after=%d minimum-increase=%d error=%v", part, retained, after, increase, err)
		}
	}
}

func legacyDerivedMetadataLargeFixture(test testing.TB) (VerifiedLegacyDerivedRangeMetadata, int) {
	test.Helper()
	proof, raw := legacyDocByteBackfillSizedFixture(test, 17<<20)
	backing := string(proof.Hash()) + strings.Repeat("unowned-hash-backing", 1<<20)
	proof.hash = ContentHash(backing[:len(proof.Hash())])
	metadata, err := PrepareLegacyDerivedRangeMetadata(context.Background(), proof, func(ctx context.Context, object DocRangeObject) error {
		if HashContent(object.Bytes) != object.Hash {
			test.Fatal("large fixture saved corrupt metadata")
		}
		runtime.KeepAlive(proof)
		runtime.KeepAlive(raw)
		return ctx.Err()
	})
	if err != nil {
		test.Fatal(err)
	}
	return metadata, len(raw)
}

func TestLegacyDerivedRangeMetadataRetainsNoTranscriptOrSaveCallback(test *testing.T) {
	if testing.Short() {
		test.Skip("17 MiB transcript retained-heap regression")
	}
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	metadata, sourceBytes := legacyDerivedMetadataLargeFixture(test)
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	retained, err := metadata.RetainedBytes()
	if err != nil || retained <= 0 || retained >= 64<<10 || sourceBytes <= 16<<20 {
		test.Fatalf("capsule accounting scaled with the transcript: retained=%d source=%d error=%v", retained, sourceBytes, err)
	}
	heapDelta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if heapDelta >= 4<<20 {
		test.Fatalf("capsule retained source bytes, hash backing, or save callback: heap-delta=%d metadata-estimate=%d source=%d", heapDelta, retained, sourceBytes)
	}
	descriptor, err := metadata.Descriptor()
	if err != nil || descriptor.StreamBytes != 17<<20 || descriptor.ChunkCount() != 34 || metadata.ObjectCount() == 0 {
		test.Fatalf("small capsule lost large transcript metadata: %+v, %v", descriptor, err)
	}
	runtime.KeepAlive(metadata)
}
