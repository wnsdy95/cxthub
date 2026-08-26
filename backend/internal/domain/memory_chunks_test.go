package domain

import (
	"strings"
	"testing"
)

func TestMemoryComponentPlanRoundtripAndPrefixDedup(t *testing.T) {
	base := strings.Repeat("résumé-memory-", 40<<10)
	first := MemoryDigest{
		SnapshotID: HashContent([]byte("memory-first")),
		Summary:    base,
		KeyFacts:   []string{"immutable identity"},
		OpenTasks:  []string{},
		Provider:   ProviderCodex,
		Fragments: []MemoryFragment{{
			SourceSnapshot: HashContent([]byte("memory-source")),
			Summary:        strings.Repeat("fragment", 12<<10),
		}},
		GraftCoverage: &MemoryGraftCoverage{
			ProjectionVersion:  MemoryProjectionVersion,
			ProjectionComplete: true,
			LineageFingerprint: HashContent([]byte("lineage-state")),
			GraftSeq:           2,
			GraftParents:       []ContentHash{HashContent([]byte("graft-parent"))},
			PinnedSources:      []ContentHash{HashContent([]byte("pinned-source"))},
		},
	}
	second := first
	second.SnapshotID = HashContent([]byte("memory-second"))
	second.Summary += strings.Repeat("tail", 32<<10)

	p1, ok, err := PlanMemoryChunks(first)
	if err != nil || !ok {
		t.Fatalf("PlanMemoryChunks(first): ok=%v err=%v", ok, err)
	}
	p2, ok, err := PlanMemoryChunks(second)
	if err != nil || !ok {
		t.Fatalf("PlanMemoryChunks(second): ok=%v err=%v", ok, err)
	}
	if p1.Manifest.Format != MemoryChunkFormatV2 {
		t.Fatalf("format=%q", p1.Manifest.Format)
	}
	for hash, body := range p2.Bodies {
		if len(body) > MemoryChunkTarget {
			t.Fatalf("chunk %s exceeds target: %d", hash, len(body))
		}
		if HashContent(body) != hash {
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
	got, err := AssembleMemoryChunks(p1.Manifest, p1.Bodies)
	if err != nil {
		t.Fatalf("AssembleMemoryChunks: %v", err)
	}
	wantHash, _ := MemoryDigestHash(first)
	gotHash, _ := MemoryDigestHash(got)
	if gotHash != wantHash {
		t.Fatalf("identity changed: got %s want %s", gotHash, wantHash)
	}
}

func TestMemoryComponentPlanKeepsLegacyManifestVersionWithoutCoverage(t *testing.T) {
	digest := MemoryDigest{
		SnapshotID: HashContent([]byte("legacy-format")),
		Summary:    strings.Repeat("legacy", 16<<10),
		Provider:   ProviderCodex,
	}
	plan, ok, err := PlanMemoryChunks(digest)
	if err != nil || !ok || plan.Manifest.Format != MemoryChunkFormatV1 {
		t.Fatalf("legacy plan: format=%q ok=%v err=%v", plan.Manifest.Format, ok, err)
	}
}

func TestMemoryComponentPlanUsesV3ForCausalParent(t *testing.T) {
	previous := HashContent([]byte("previous-memory"))
	digest := MemoryDigest{
		SnapshotID: HashContent([]byte("causal-format")), PreviousMemoryHash: previous,
		Summary: strings.Repeat("causal", 16<<10), Provider: ProviderCodex,
		GraftCoverage: &MemoryGraftCoverage{ProjectionVersion: MemoryProjectionVersion},
	}
	plan, ok, err := PlanMemoryChunks(digest)
	if err != nil || !ok || plan.Manifest.Format != MemoryChunkFormatV3 {
		t.Fatalf("causal plan: format=%q ok=%v err=%v", plan.Manifest.Format, ok, err)
	}
	got, err := AssembleMemoryChunks(plan.Manifest, plan.Bodies)
	if err != nil || got.PreviousMemoryHash != previous {
		t.Fatalf("causal roundtrip: previous=%s err=%v", got.PreviousMemoryHash, err)
	}
}

func TestMemoryComponentPlanKeepsSmallDigestMonolithic(t *testing.T) {
	_, ok, err := PlanMemoryChunks(MemoryDigest{Summary: "small", Provider: ProviderClaude})
	if err != nil || ok {
		t.Fatalf("small digest should stay monolithic: ok=%v err=%v", ok, err)
	}
}
