package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

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

func TestMemoryComponentPlanKeepsSmallDigestMonolithic(t *testing.T) {
	_, ok, err := PlanMemoryChunks(MemoryDigest{Summary: "small", Provider: ProviderClaude})
	if err != nil || ok {
		t.Fatalf("small digest should stay monolithic: ok=%v err=%v", ok, err)
	}
}

func TestTypedMemoryComponentRoundTrip(t *testing.T) {
	d := MemoryDigest{SnapshotID: HashContent([]byte("typed")), ClaimsVersion: 1, Summary: strings.Repeat("archive", 12000), Fragments: []MemoryFragment{{SourceSnapshot: HashContent([]byte("source")), Claims: []MemoryClaim{{Kind: "rationale", Text: "Historical reason."}}}}}
	plan, ok, err := PlanMemoryChunks(d)
	if err != nil || !ok || plan.Manifest.Format != MemoryChunkFormatV4 {
		t.Fatalf("typed plan=%+v ok=%v err=%v", plan.Manifest, ok, err)
	}
	raw, _ := json.Marshal(plan.Manifest)
	man, recognized, err := ParseMemoryChunkManifest(raw)
	if err != nil || !recognized {
		t.Fatalf("parse=%v %v", recognized, err)
	}
	got, err := AssembleMemoryChunks(man, plan.Bodies)
	wantHash, _ := MemoryDigestHash(d)
	gotHash, _ := MemoryDigestHash(got)
	if err != nil || gotHash != wantHash {
		t.Fatalf("roundtrip got %s want %s err=%v", gotHash, wantHash, err)
	}
	man.Format = MemoryChunkFormatV3
	raw, _ = json.Marshal(man)
	if _, _, err := ParseMemoryChunkManifest(raw); err == nil {
		t.Fatal("typed version hidden in legacy manifest")
	}
}
