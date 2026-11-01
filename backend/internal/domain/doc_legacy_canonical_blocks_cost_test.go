package domain

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"testing"
)

func legacyCanonicalBlocksCostEvent(count int) CIREvent {
	blocks := make([]ContentBlock, count)
	for position := range blocks {
		blocks[position] = ContentBlock{Type: "text", Text: strconv.Itoa(position) + " <&>\n한🙂\u2028\u2029\"\\"}
	}
	created := json.Number("1787683260.123456789")
	return CIREvent{
		Kind:             EventMessage,
		Seq:              17,
		Role:             RoleAssistant,
		Blocks:           blocks,
		ProviderMetadata: &ProviderMetadata{TurnID: "blocks-cost", CreateTime: &created},
	}
}

func legacyCanonicalBlocksCostDigest(test testing.TB, encoding *LegacyCanonicalEncoding) ([sha256.Size]byte, int64) {
	test.Helper()
	digest := sha256.New()
	var length int64
	for step := 0; step < 1<<16; step++ {
		chunk, complete, err := encoding.Next(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		if len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
			test.Fatalf("oversized block encoding chunk: len=%d cap=%d", len(chunk), cap(chunk))
		}
		digest.Write(chunk)
		length += int64(len(chunk))
		if complete {
			var sum [sha256.Size]byte
			copy(sum[:], digest.Sum(nil))
			return sum, length
		}
	}
	test.Fatal("block encoding did not terminate")
	return [sha256.Size]byte{}, 0
}

func BenchmarkLegacyCanonicalBlocksInitialProjection(benchmark *testing.B) {
	benchmark.StopTimer()
	benchmark.Log("Only projection plus the pending constructor are timed; source CIREvent setup, retained snapshots, and streamed digest checks are untimed. Typed inspect still visits all blocks; retained estimates include referenced source storage, not just new allocations.")
	ctx := context.Background()
	for _, count := range []int{1 << 10, 16 << 10, 64 << 10} {
		event := legacyCanonicalBlocksCostEvent(count)
		original, err := canonicalEventJSONObject(event)
		if err != nil {
			benchmark.Fatal(err)
		}
		originalEncoding, err := newLegacyCanonicalEncodingPending(ctx, original)
		if err != nil {
			benchmark.Fatal(err)
		}
		wantDigest, wantLength := legacyCanonicalBlocksCostDigest(benchmark, originalEncoding)
		for _, projection := range []struct {
			name    string
			project func(CIREvent) (map[string]any, error)
		}{
			{name: "canonicalEventJSONObject+Pending", project: canonicalEventJSONObject},
			{name: "legacyCanonicalEventJSONObject+Pending", project: legacyCanonicalEventJSONObject},
		} {
			benchmark.Run(fmt.Sprintf("blocks=%d/%s", count, projection.name), func(benchmark *testing.B) {
				benchmark.StopTimer()
				value, err := projection.project(event)
				if err != nil {
					benchmark.Fatal(err)
				}
				encoding, err := newLegacyCanonicalEncodingPending(ctx, value)
				if err != nil {
					benchmark.Fatal(err)
				}
				retained, err := encoding.RetainedBytes()
				if err != nil {
					benchmark.Fatal(err)
				}
				mapKeys := encoding.mapKeys.length()
				gotDigest, gotLength := legacyCanonicalBlocksCostDigest(benchmark, encoding)
				if gotDigest != wantDigest || gotLength != wantLength {
					benchmark.Fatal("typed block projection changed the original streamed canonical output")
				}
				benchmark.ReportAllocs()
				benchmark.ResetTimer()
				benchmark.StartTimer()
				for operation := 0; operation < benchmark.N; operation++ {
					value, err := projection.project(event)
					if err != nil {
						benchmark.Fatal(err)
					}
					encoding, err := newLegacyCanonicalEncodingPending(ctx, value)
					if err != nil {
						benchmark.Fatal(err)
					}
					runtime.KeepAlive(encoding)
				}
				benchmark.StopTimer()
				benchmark.ReportMetric(float64(retained), "constructor-retained-B")
				benchmark.ReportMetric(float64(mapKeys), "constructor-mapkeys")
			})
		}
	}
}

func TestLegacyCanonicalBlocksInitialProjectionKeepsTypedStorage(test *testing.T) {
	ctx := context.Background()
	for _, count := range []int{1, 4 << 10} {
		test.Run(fmt.Sprintf("blocks=%d", count), func(test *testing.T) {
			event := legacyCanonicalBlocksCostEvent(count)
			value, err := legacyCanonicalEventJSONObject(event)
			if err != nil {
				test.Fatal(err)
			}
			blocks, ok := value["blocks"].([]ContentBlock)
			if !ok || len(blocks) != count || &blocks[0] != &event.Blocks[0] {
				test.Fatal("initial projection expanded or copied the source content blocks")
			}
			encoding, err := newLegacyCanonicalEncodingPending(ctx, value)
			if err != nil {
				test.Fatal(err)
			}
			root, ok := encoding.root.(map[string]any)
			if !ok {
				test.Fatalf("pending constructor replaced the event root with %T", encoding.root)
			}
			blocks, ok = root["blocks"].([]ContentBlock)
			if !ok || len(blocks) != count || &blocks[0] != &event.Blocks[0] {
				test.Fatal("pending constructor expanded or copied the source content blocks")
			}
			if encoding.mapKeys.length() != 2 {
				test.Fatalf("constructor prepared block map keys: got=%d want=2 root/metadata maps", encoding.mapKeys.length())
			}
			original, err := canonicalEventJSONObject(event)
			if err != nil {
				test.Fatal(err)
			}
			originalEncoding, err := newLegacyCanonicalEncodingPending(ctx, original)
			if err != nil {
				test.Fatal(err)
			}
			if originalEncoding.mapKeys.length() != count+2 {
				test.Fatalf("original-path fixture did not prepare each block map: got=%d want=%d", originalEncoding.mapKeys.length(), count+2)
			}
			wantDigest, wantLength := legacyCanonicalBlocksCostDigest(test, originalEncoding)
			gotDigest, gotLength := legacyCanonicalBlocksCostDigest(test, encoding)
			if gotDigest != wantDigest || gotLength != wantLength {
				test.Fatal("typed block projection changed the original streamed canonical output")
			}
		})
	}
}
