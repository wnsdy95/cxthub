package memoryquery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type projectionTestSource struct {
	snapshots    []domain.Snapshot
	memories     map[domain.ContentHash]domain.MemoryDigest
	onMemory     func()
	lists, reads int
}

func (source *projectionTestSource) ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error) {
	source.lists++
	return append([]domain.Snapshot(nil), source.snapshots...), nil
}
func (source *projectionTestSource) GetMemory(_ context.Context, _, hash domain.ContentHash) (domain.MemoryDigest, error) {
	source.reads++
	if source.onMemory != nil {
		source.onMemory()
	}
	if digest, ok := source.memories[hash]; ok {
		return digest, nil
	}
	return domain.MemoryDigest{}, domain.ErrNotFound
}
func projectionSource() (*projectionTestSource, domain.ContentHash, domain.ContentHash) {
	repo, base, tip := projectionTestHash("repo"), projectionTestHash("base"), projectionTestHash("tip")
	source := &projectionTestSource{memories: map[domain.ContentHash]domain.MemoryDigest{}}
	for _, entry := range []struct {
		id   domain.ContentHash
		text string
	}{{base, "BASE DECISION"}, {tip, "TIP DECISION"}} {
		digest := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: entry.id, Summary: entry.text})
		hash, _ := domain.MemoryDigestHash(digest)
		source.memories[hash] = digest
		source.snapshots = append(source.snapshots, domain.Snapshot{ID: entry.id, RepoID: repo, MemoryHash: hash})
	}
	return source, repo, tip
}
func TestProjectMemoryRetriesConcurrentGraftChange(test *testing.T) {
	source, repo, tip := projectionSource()
	source.onMemory = func() {
		source.onMemory = nil
		source.snapshots[1].GraftParents = []domain.ContentHash{source.snapshots[0].ID}
		source.snapshots[1].GraftSeq++
	}
	got, err := ProjectMemory(projectionTestContext(), source, repo, tip)
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(got.Digest.Summary, "BASE DECISION") || source.lists != 4 {
		test.Fatalf("inconsistent projection: %+v lists=%d", got, source.lists)
	}
	if source.reads != 2 {
		test.Fatalf("immutable memory loaded repeatedly: %d", source.reads)
	}
}
func TestProjectMemoryRejectsBrokenLineage(test *testing.T) {
	for _, kind := range []string{"missing snapshot", "cycle", "foreign repository", "missing memory", "corrupt memory"} {
		test.Run(kind, func(test *testing.T) {
			source, repo, tip := projectionSource()
			switch kind {
			case "missing snapshot":
				source.snapshots[1].GraftParents = []domain.ContentHash{projectionTestHash("absent")}
			case "cycle":
				source.snapshots[1].GraftParents = []domain.ContentHash{tip}
			case "foreign repository":
				source.snapshots[1].RepoID = projectionTestHash("foreign")
			case "missing memory":
				delete(source.memories, source.snapshots[1].MemoryHash)
			case "corrupt memory":
				source.memories[source.snapshots[1].MemoryHash] = domain.MemoryDigest{SnapshotID: tip, Summary: "tampered"}
			}
			if _, err := ProjectMemory(projectionTestContext(), source, repo, tip); err == nil {
				test.Fatal("invalid lineage presented as complete")
			}
		})
	}
}
func TestProjectMemoryInvalidatesTransitiveCoverage(test *testing.T) {
	source, repo, tip := projectionSource()
	source.snapshots[1].Parents = []domain.ContentHash{source.snapshots[0].ID}
	reader := &ProjectionReader{source: source, repoID: repo, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	if _, err := reader.readState(projectionTestContext(), tip); err != nil {
		test.Fatal(err)
	}
	fingerprint, ok := newMemoryProjectionFingerprinter(projectionTestContext(), reader).root(source.snapshots[1])
	if !ok {
		test.Fatal("incomplete fixture")
	}
	digest := domain.MergeDigests(source.memories[source.snapshots[0].MemoryHash], source.memories[source.snapshots[1].MemoryHash])
	digest.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: domain.MemoryProjectionVersion, ProjectionComplete: true, LineageFingerprint: fingerprint}
	hash, _ := domain.MemoryDigestHash(digest)
	source.memories[hash] = digest
	source.snapshots[1].MemoryHash = hash
	first, err := ProjectMemory(projectionTestContext(), source, repo, tip)
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(first.Digest.Summary, "LATE") {
		test.Fatal("invalid fixture")
	}
	late := projectionTestHash("late")
	lateDigest := domain.MemoryDigest{SnapshotID: late, Summary: "LATE DECISION"}
	digestHash, _ := domain.MemoryDigestHash(lateDigest)
	source.memories[digestHash] = lateDigest
	source.snapshots = append(source.snapshots, domain.Snapshot{ID: late, RepoID: repo, MemoryHash: digestHash})
	source.snapshots[0].GraftParents = []domain.ContentHash{late}
	source.snapshots[0].GraftSeq++
	next, err := ProjectMemory(projectionTestContext(), source, repo, tip)
	if err != nil {
		test.Fatal(err)
	}
	for _, want := range []string{"BASE DECISION", "TIP DECISION", "LATE DECISION"} {
		if !strings.Contains(next.Digest.Summary, want) {
			test.Fatalf("stale transitive projection omitted %s", want)
		}
	}
	if first.StateHash == next.StateHash {
		test.Fatal("dependency change did not invalidate cursor state")
	}
}
func TestProjectMemoryBoundsAndCancellation(test *testing.T) {
	source, repo, _ := projectionSource()
	ctx, cancel := context.WithCancel(projectionTestContext())
	cancel()
	if _, err := ProjectMemory(ctx, source, repo, source.snapshots[0].ID); !errors.Is(err, context.Canceled) {
		test.Fatalf("cancellation: %v", err)
	}
	if source.lists != 0 {
		test.Fatal("cancelled read touched storage")
	}
	var previous domain.ContentHash
	for index := 0; index <= MaxProjectionSnapshots; index++ {
		id := projectionTestHash(fmt.Sprint("depth", index))
		snap := domain.Snapshot{ID: id, RepoID: repo}
		if previous != "" {
			snap.Parents = []domain.ContentHash{previous}
		}
		source.snapshots = append(source.snapshots, snap)
		previous = id
	}
	if _, err := ProjectMemory(projectionTestContext(), source, repo, previous); err == nil || !strings.Contains(err.Error(), "exceeds") {
		test.Fatalf("unbounded graph: %v", err)
	}
	reader := &ProjectionReader{source: source, repoID: repo, objectLimit: 1, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	if _, err := reader.GetMemory(projectionTestContext(), source.snapshots[0].MemoryHash); err == nil || !strings.Contains(err.Error(), "exceeds") {
		test.Fatalf("unbounded memory: %v", err)
	}
}

func projectionTestHash(value string) domain.ContentHash {
	sum := sha256.Sum256([]byte(value))
	return domain.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
}

func projectionTestContext() context.Context {
	return context.Background()
}
