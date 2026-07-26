package domain

import (
	"bytes"
	"testing"
)

func TestPlanDocChunksFallsBackForOversizedSingleEvent(t *testing.T) {
	prefix := []byte(`{"envelope":{},"events":[{"text":"`)
	suffix := []byte(`"}]}`)
	canonical := make([]byte, 0, len(prefix)+MaxPortableChunkBytes+1+len(suffix))
	canonical = append(canonical, prefix...)
	canonical = append(canonical, bytes.Repeat([]byte{'x'}, MaxPortableChunkBytes+1)...)
	canonical = append(canonical, suffix...)

	if _, ok := PlanDocChunks(canonical); ok {
		t.Fatal("oversized single event must use the full-doc fallback")
	}
}
