package chunkcas

import (
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestMemoryComponentPlanRoundtripAndPrefixDedup(t *testing.T) {
	base := strings.Repeat("résumé-memory-", 40<<10)
	first := domain.MemoryDigest{
		SnapshotID: domain.HashContent([]byte("memory-first")),
		Summary:    base,
		KeyFacts:   []string{"immutable identity"},
		OpenTasks:  []string{},
		Provider:   domain.ProviderCodex,
		Fragments: []domain.MemoryFragment{{
			SourceSnapshot: domain.HashContent([]byte("memory-source")),
			Summary:        strings.Repeat("fragment", 12<<10),
		}},
	}
	second := first
	second.SnapshotID = domain.HashContent([]byte("memory-second"))
	second.Summary += strings.Repeat("tail", 32<<10)

	p1, ok, err := PlanMemory(first)
	if err != nil || !ok {
		t.Fatalf("PlanMemory(first): ok=%v err=%v", ok, err)
	}
	p2, ok, err := PlanMemory(second)
	if err != nil || !ok {
		t.Fatalf("PlanMemory(second): ok=%v err=%v", ok, err)
	}
	if p1.Manifest.Format != MemoryFormatV1 {
		t.Fatalf("format=%q", p1.Manifest.Format)
	}
	for hash, body := range p2.Bodies {
		if len(body) > MemoryChunkTarget {
			t.Fatalf("chunk %s exceeds target: %d", hash, len(body))
		}
		if domain.HashContent(body) != hash {
			t.Fatalf("chunk body/hash mismatch: %s", hash)
		}
	}
	shared := 0
	for hash := range p1.Bodies {
		if _, exists := p2.Bodies[hash]; exists {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("growing memory did not reuse any component chunk")
	}
	got, err := AssembleMemory(p1.Manifest, p1.Bodies)
	if err != nil {
		t.Fatalf("AssembleMemory: %v", err)
	}
	wantHash, _ := domain.MemoryDigestHash(first)
	gotHash, _ := domain.MemoryDigestHash(got)
	if gotHash != wantHash {
		t.Fatalf("identity changed: got %s want %s", gotHash, wantHash)
	}
}

func TestMemoryComponentPlanKeepsSmallDigestMonolithic(t *testing.T) {
	_, ok, err := PlanMemory(domain.MemoryDigest{Summary: "small", Provider: domain.ProviderClaude})
	if err != nil || ok {
		t.Fatalf("small digest should stay monolithic: ok=%v err=%v", ok, err)
	}
}
