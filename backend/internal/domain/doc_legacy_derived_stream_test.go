package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestLegacyDerivedMetadataFrozenFingerprint(tests *testing.T) {
	fixtures := []struct {
		name  string
		count int
		large bool
		want  ContentHash
	}{
		{name: "empty", count: 0, want: "sha256:1840b03939874ad97dc2582d30eca4e5093ef43ac0d7bee3ccb636f56a0217ce"},
		{name: "one", count: 1, want: "sha256:b3d3387a1464c5fc15591ecd720eca64b9b052dc1fe2e7ad086d6b81e21b5cc9"},
		{name: "event-pages", count: DocRangePageEvents + 3, want: "sha256:1ce99541a5b530826077824e5b6c94e441e022d0c47266d461159c6a68273d17"},
		{name: "several-pages", count: 3*DocRangePageEvents + 1, want: "sha256:b930081ec5bb0233aef7bb6bd87d3baa49ce20da8bb618ed48ce005fa8addc43"},
		{name: "byte-blocks", large: true, want: "sha256:2537c93cb7c0b4406bad846095e95e0ee62fb1be2a5068a6e5ee017eede1ffa6"},
		{name: "directory-level", count: DocRangePageEvents*DocRangeDirectoryFanout + 1, want: "sha256:68a0569ce0495f9b4e028da66b54a62315fd379afa0a3852306683fa5c315d5b"},
	}
	for _, fixture := range fixtures {
		tests.Run(fixture.name, func(test *testing.T) {
			var proof VerifiedSessionDoc
			if fixture.large {
				proof, _ = legacyDocByteGenerationLargeFixture(test)
			} else {
				proof, _ = legacyDocRangeGenerationFixture(test, fixture.count)
			}
			generation, objects, err := BuildLegacyDerivedDocByteRangeGeneration(context.Background(), proof)
			if err != nil {
				test.Fatal(err)
			}
			descriptor, err := generation.Encode()
			if err != nil {
				test.Fatal(err)
			}
			fingerprint := struct {
				Descriptor json.RawMessage `json:"descriptor"`
				Objects    []ContentHash   `json:"objects"`
			}{Descriptor: descriptor, Objects: make([]ContentHash, 0, len(objects))}
			for _, object := range objects {
				if HashContent(object.Bytes) != object.Hash {
					test.Fatal("object bytes differ from their identity")
				}
				fingerprint.Objects = append(fingerprint.Objects, object.Hash)
			}
			raw, err := json.Marshal(fingerprint)
			if err != nil {
				test.Fatal(err)
			}
			if got := HashContent(raw); got != fixture.want {
				test.Fatalf("frozen descriptor/object order = %s, want %s", got, fixture.want)
			}
		})
	}
}

func TestLegacyRangeEmissionStopsBeforeUnrequestedPage(tests *testing.T) {
	for _, failure := range []string{"save", "cancel", "corrupt-tail"} {
		tests.Run(failure, func(test *testing.T) {
			proof, _ := legacyDocRangeGenerationFixture(test, 2*DocRangePageEvents+3)
			plan, err := proof.PlanReadIndexContext(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			plan.index.Events[DocRangePageEvents].Hash = HashContent([]byte("corrupt later event metadata"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads, saves := 0, 0
			saveFailure := errors.New("stop at first metadata object")
			generation, err := emitLegacyDocRangeGeneration(ctx, proof.DocumentRef(), plan, func(offset, length int) ([]byte, error) {
				reads++
				return proof.EventStreamRange(ctx, offset, length)
			}, func(object DocRangeObject) error {
				saves++
				if reads != DocRangePageEvents || HashContent(object.Bytes) != object.Hash {
					test.Fatal("metadata emission preceded validation or read an unrequested later page")
				}
				if failure == "save" {
					return saveFailure
				}
				if failure == "cancel" {
					cancel()
				}
				return nil
			})
			want, expectedReads := saveFailure, DocRangePageEvents
			if failure == "cancel" {
				want = context.Canceled
			}
			if failure == "corrupt-tail" {
				want, expectedReads = ErrIntegrity, DocRangePageEvents+1
			}
			if !errors.Is(err, want) || !reflect.DeepEqual(generation, DocRangeGeneration{}) || reads != expectedReads || saves != 1 {
				test.Fatalf("partial emission escaped its failure: reads=%d saves=%d generation=%+v error=%v", reads, saves, generation, err)
			}
		})
	}
}

func TestLegacyPreparedMetadataCorruptionHasRetryableIntegrityIdentity(test *testing.T) {
	metadata, saved := legacyDerivedMetadataFixture(test, DocRangePageEvents+1)
	for _, corruption := range []string{"bytes", "length", "loader"} {
		test.Run(corruption, func(test *testing.T) {
			object, err := metadata.Object(context.Background(), 0, func(context.Context, int) ([]byte, error) {
				body := append([]byte(nil), saved[0].Bytes...)
				switch corruption {
				case "bytes":
					body[0] ^= 1
				case "length":
					body = body[:len(body)-1]
				case "loader":
					return nil, ErrIntegrity
				}
				return body, nil
			})
			if !errors.Is(err, ErrPreparedDocRangeObject) || !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(object, DocRangeObject{}) {
				test.Fatalf("prepared corruption lost retry identity, integrity compatibility or empty output: %+v, %v", object, err)
			}
		})
	}
	if errors.Is(ErrIntegrity, ErrPreparedDocRangeObject) {
		test.Fatal("original source integrity was incorrectly made retryable")
	}
}
