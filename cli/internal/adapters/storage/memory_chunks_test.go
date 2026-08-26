package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func largeMemory(label string, extra int) domain.MemoryDigest {
	return domain.MemoryDigest{
		SnapshotID: domain.HashContent([]byte("snapshot-" + label)),
		Summary:    strings.Repeat("shared-memory-prefix-", 12<<10) + strings.Repeat(label, extra),
		KeyFacts:   []string{"memory identity is immutable"},
		OpenTasks:  []string{},
		Provider:   domain.ProviderCodex,
		Fragments: []domain.MemoryFragment{{
			SourceSnapshot: domain.HashContent([]byte("source-" + label)),
			Summary:        strings.Repeat("fragment-", 10<<10),
		}},
		GraftCoverage: &domain.MemoryGraftCoverage{
			ProjectionVersion:  domain.MemoryProjectionVersion,
			ProjectionComplete: true,
			LineageFingerprint: domain.HashContent([]byte("lineage-" + label)),
			GraftSeq:           3,
			GraftParents:       []domain.ContentHash{domain.HashContent([]byte("graft-" + label))},
			PinnedSources:      []domain.ContentHash{domain.HashContent([]byte("pinned-" + label))},
		},
	}
}

func memoryChunkFiles(t *testing.T, root string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	dir := filepath.Join(root, ".cxt", "objects", "memory_chunks")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = info.Size()
	}
	return out
}

func TestMemoryChunkRoundtripPrefixDedupAndCorruption(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := NewFileStore(root)

	first := largeMemory("first", 8<<10)
	firstHash, err := st.PutMemory(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetMemory(ctx, firstHash)
	if err != nil {
		t.Fatalf("GetMemory(chunked): %v", err)
	}
	gotHash, _ := domain.MemoryDigestHash(got)
	if gotHash != firstHash {
		t.Fatalf("roundtrip identity changed: %s != %s", gotHash, firstHash)
	}
	before := memoryChunkFiles(t, root)
	if len(before) < 2 {
		t.Fatalf("expected component chunks, got %d", len(before))
	}

	second := first
	second.SnapshotID = domain.HashContent([]byte("snapshot-second"))
	second.Summary += strings.Repeat("tail", 40<<10)
	secondHash, err := st.PutMemory(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	after := memoryChunkFiles(t, root)
	shared := 0
	for name := range before {
		if _, ok := after[name]; ok {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("memory growth did not reuse prefix chunks")
	}
	if got, err := st.GetMemory(ctx, secondHash); err != nil || got.Summary != second.Summary {
		t.Fatalf("second memory roundtrip: err=%v", err)
	}

	raw, err := readCxtFile(st.objectPath("memories", firstHash))
	if err != nil {
		t.Fatal(err)
	}
	manifest, isManifest, err := chunkcas.ParseMemoryManifest(raw)
	if err != nil || !isManifest || len(manifest.SummaryChunks) == 0 {
		t.Fatalf("stored memory is not a manifest: %+v is=%v err=%v", manifest, isManifest, err)
	}
	victim := st.objectPath("memory_chunks", manifest.SummaryChunks[0])
	if err := os.WriteFile(victim, docCompress([]byte("corrupt")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMemory(ctx, firstHash); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("corrupt component was not rejected: %v", err)
	}
}

func TestMemoryChunkReadValidatesInlineHashReferences(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := NewFileStore(root)
	digest := largeMemory("invalid-reference", 8<<10)
	digest.SnapshotID = domain.ContentHash("invalid")
	plan, ok, err := chunkcas.PlanMemory(digest)
	if err != nil || !ok {
		t.Fatalf("PlanMemory: ok=%v err=%v", ok, err)
	}
	for hash, body := range plan.Bodies {
		if err := writeAtomic(st.objectPath("memory_chunks", hash), docCompress(body)); err != nil {
			t.Fatal(err)
		}
	}
	hash, _ := domain.MemoryDigestHash(digest)
	manifest, _ := json.Marshal(plan.Manifest)
	if err := writeAtomic(st.objectPath("memories", hash), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMemory(ctx, hash); err == nil {
		t.Fatal("chunked memory accepted an invalid snapshot reference")
	}
}

func TestRepackMemoriesLegacyLosslessAndIdempotent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := NewFileStore(root)
	digests := []domain.MemoryDigest{largeMemory("legacy-a", 16<<10), largeMemory("legacy-b", 32<<10)}
	var hashes []domain.ContentHash
	for _, digest := range digests {
		data, err := json.Marshal(digest)
		if err != nil {
			t.Fatal(err)
		}
		hash, _ := domain.MemoryDigestHash(digest)
		if err := writeAtomic(st.objectPath("memories", hash), data); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, hash)
	}
	objects := filepath.Join(root, ".cxt", "objects")
	before := dirSize(t, objects)
	converted, saved, err := st.RepackMemories()
	if err != nil {
		t.Fatalf("RepackMemories: %v", err)
	}
	if converted != len(digests) {
		t.Fatalf("converted=%d want %d", converted, len(digests))
	}
	after := dirSize(t, objects)
	if after >= before || saved != before-after {
		t.Fatalf("repack accounting before=%d after=%d saved=%d", before, after, saved)
	}
	for i, hash := range hashes {
		got, err := st.GetMemory(ctx, hash)
		if err != nil || got.Summary != digests[i].Summary {
			t.Fatalf("repacked memory %d: err=%v", i, err)
		}
	}
	if converted, _, err := st.RepackMemories(); err != nil || converted != 0 {
		t.Fatalf("second repack converted=%d err=%v", converted, err)
	}
}
