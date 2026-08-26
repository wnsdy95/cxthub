package chunkcas

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestPlanDocV2SplitsOversizedSingleEvent(t *testing.T) {
	prefix := []byte(`{"envelope":{},"events":[{"text":"`)
	suffix := []byte(`"}]}`)
	canonical := make([]byte, 0, len(prefix)+MaxPortableChunkBytes+1+len(suffix))
	canonical = append(canonical, prefix...)
	canonical = append(canonical, bytes.Repeat([]byte{'x'}, MaxPortableChunkBytes+1)...)
	canonical = append(canonical, suffix...)

	plan, ok := PlanDoc(canonical)
	if !ok {
		t.Fatal("v2 must chunk an oversized event without a full-doc fallback")
	}
	if plan.Manifest.Format != FormatV2 || len(plan.Order) < 2 {
		t.Fatalf("format=%q chunks=%d, want v2 multi-chunk", plan.Manifest.Format, len(plan.Order))
	}
	chunks := make([][]byte, 0, len(plan.Order))
	for _, hash := range plan.Order {
		body := plan.Bodies[hash]
		if len(body) > ChunkTarget {
			t.Fatalf("chunk %s is %d bytes, want <= %d", hash, len(body), ChunkTarget)
		}
		chunks = append(chunks, body)
	}
	got, err := AssembleChunks(plan.Manifest, chunks, domain.HashContent(canonical))
	if err != nil || !bytes.Equal(got, canonical) {
		t.Fatalf("v2 roundtrip err=%v equal=%v", err, bytes.Equal(got, canonical))
	}
}

func TestV1ManifestAndEmptyWireFormatRemainReadable(t *testing.T) {
	canonical := []byte(`{"envelope":{},"events":[{"text":"a"},{"text":"b"}]}`)
	plan, ok := PlanDocV1(canonical)
	if !ok {
		t.Fatal("v1 plan unavailable")
	}
	encoded, _ := json.Marshal(plan.Manifest)
	parsed, ok := ParseManifest(encoded)
	if !ok || parsed.Format != FormatV1 {
		t.Fatalf("parsed=%+v ok=%v", parsed, ok)
	}
	chunks := make([][]byte, 0, len(plan.Order))
	for _, hash := range plan.Order {
		chunks = append(chunks, plan.Bodies[hash])
	}
	parsed.Format = ""
	got, err := AssembleChunks(parsed, chunks, domain.HashContent(canonical))
	if err != nil || !bytes.Equal(got, canonical) {
		t.Fatalf("empty v1 wire roundtrip err=%v", err)
	}
	parsed.Format = "cxt-doc-chunks-v999"
	if _, err := AssembleChunks(parsed, chunks, domain.HashContent(canonical)); err == nil {
		t.Fatal("unknown manifest format was accepted")
	}
}

func TestPlanDocV1CompatibilityStillFallsBack(t *testing.T) {
	prefix := []byte(`{"envelope":{},"events":[{"text":"`)
	suffix := []byte(`"}]}`)
	canonical := append(append(append([]byte{}, prefix...), bytes.Repeat([]byte{'x'}, MaxPortableChunkBytes+1)...), suffix...)
	if _, ok := PlanDocV1(canonical); ok {
		t.Fatal("v1 cannot transport an event above its single-chunk bound")
	}
}
