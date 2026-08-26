package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func largeServerMemory(label string, extra int) domain.MemoryDigest {
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

func serverTreeSize(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	if err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	}); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return total
}

func TestFSMemoryChunkRoundtripPrefixDedupAndCorruption(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := rlHash('3')
	first := largeServerMemory("first", 8<<10)
	hash, err := st.PutMemory(ctx, repo, first)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetMemory(ctx, repo, hash)
	if err != nil {
		t.Fatalf("GetMemory(chunked): %v", err)
	}
	gotHash, _ := domain.MemoryDigestHash(got)
	if gotHash != hash {
		t.Fatalf("roundtrip identity changed: %s != %s", gotHash, hash)
	}

	second := first
	second.SnapshotID = domain.HashContent([]byte("snapshot-second"))
	second.Summary += strings.Repeat("tail", 40<<10)
	if _, err := st.PutMemory(ctx, repo, second); err != nil {
		t.Fatal(err)
	}
	chunkDir := filepath.Join(st.repoDir(repo), "objects", "memory_chunks")
	entries, err := os.ReadDir(chunkDir)
	if err != nil || len(entries) < 2 {
		t.Fatalf("component chunks=%d err=%v", len(entries), err)
	}
	raw, err := os.ReadFile(st.memPath(repo, hash))
	if err != nil {
		t.Fatal(err)
	}
	manifest, isManifest, err := domain.ParseMemoryChunkManifest(raw)
	if err != nil || !isManifest || len(manifest.SummaryChunks) == 0 {
		t.Fatalf("stored memory is not a manifest: %+v is=%v err=%v", manifest, isManifest, err)
	}
	if err := os.WriteFile(st.memoryChunkPath(repo, manifest.SummaryChunks[0]), docCompress([]byte("corrupt")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMemory(ctx, repo, hash); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("corrupt component was not rejected: %v", err)
	}
}

func TestFSMemoryChunkReadValidatesInlineHashReferences(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := rlHash('5')
	digest := largeServerMemory("invalid-reference", 8<<10)
	digest.SnapshotID = domain.ContentHash("invalid")
	plan, ok, err := domain.PlanMemoryChunks(digest)
	if err != nil || !ok {
		t.Fatalf("PlanMemoryChunks: ok=%v err=%v", ok, err)
	}
	for hash, body := range plan.Bodies {
		if err := writeAtomic(st.memoryChunkPath(repo, hash), docCompress(body)); err != nil {
			t.Fatal(err)
		}
	}
	hash, _ := domain.MemoryDigestHash(digest)
	manifest, _ := json.Marshal(plan.Manifest)
	if err := writeAtomic(st.memPath(repo, hash), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMemory(ctx, repo, hash); err == nil {
		t.Fatal("chunked memory accepted an invalid snapshot reference")
	}
}

func TestFSRepackMemoriesRemovesOnlyPointerBackedMeta(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := NewFSStore(root)
	repo := rlHash('4')
	digests := []domain.MemoryDigest{largeServerMemory("legacy-a", 16<<10), largeServerMemory("legacy-b", 32<<10)}
	var hashes []domain.ContentHash
	for _, digest := range digests {
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: digest.SnapshotID, RepoID: repo, DocHash: digest.SnapshotID}); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(digest)
		if err != nil {
			t.Fatal(err)
		}
		hash, _ := domain.MemoryDigestHash(digest)
		if err := writeAtomic(st.memPath(repo, hash), data); err != nil {
			t.Fatal(err)
		}
		if err := st.PutMemoryMeta(ctx, repo, digest); err != nil {
			t.Fatal(err)
		}
		if err := st.SetSnapshotMemory(ctx, repo, digest.SnapshotID, hash); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, hash)
	}
	legacyOnly := domain.MemoryDigest{SnapshotID: domain.HashContent([]byte("pointerless")), Summary: "legacy only", Provider: domain.ProviderClaude}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: legacyOnly.SnapshotID, RepoID: repo, DocHash: legacyOnly.SnapshotID}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutMemoryMeta(ctx, repo, legacyOnly); err != nil {
		t.Fatal(err)
	}

	before := serverTreeSize(t, st.repoDir(repo))
	converted, saved, err := st.RepackMemories()
	if err != nil {
		t.Fatalf("RepackMemories: %v", err)
	}
	if converted != len(digests) {
		t.Fatalf("converted=%d want %d", converted, len(digests))
	}
	after := serverTreeSize(t, st.repoDir(repo))
	if after >= before || saved != before-after {
		t.Fatalf("repack accounting before=%d after=%d saved=%d", before, after, saved)
	}
	for i, digest := range digests {
		if _, err := st.GetMemoryMeta(ctx, repo, digest.SnapshotID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("redundant memmeta %d was retained: %v", i, err)
		}
		got, err := st.GetMemory(ctx, repo, hashes[i])
		if err != nil || got.Summary != digest.Summary {
			t.Fatalf("repacked memory %d: err=%v", i, err)
		}
	}
	if got, err := st.GetMemoryMeta(ctx, repo, legacyOnly.SnapshotID); err != nil || got.Summary != legacyOnly.Summary {
		t.Fatalf("pointerless legacy metadata was removed: %+v err=%v", got, err)
	}
	if converted, _, err := st.RepackMemories(); err != nil || converted != 0 {
		t.Fatalf("second repack converted=%d err=%v", converted, err)
	}
}
