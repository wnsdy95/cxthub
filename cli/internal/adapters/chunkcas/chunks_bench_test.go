package chunkcas

import (
	"strings"
	"testing"
)

func BenchmarkLargeChunkPlan(b *testing.B) {
	raw := []byte(`{"envelope":{"cir_version":"1"},"events":[{"seq":0,"kind":"message","role":"user","blocks":[{"type":"text","text":"` + strings.Repeat("context payload ", 1<<20) + `"}]}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := PlanDoc(raw); !ok {
			b.Fatal("not planned")
		}
	}
}
