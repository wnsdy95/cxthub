package memoryquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestProjectionCacheEvictsAndRevalidates(test *testing.T) {
	source, repo, _ := projectionSource()
	firstHash, secondHash := source.snapshots[0].MemoryHash, source.snapshots[1].MemoryHash
	raw, _ := json.Marshal(source.memories[firstHash])
	reader := &ProjectionReader{source: source, repoID: repo, cacheLimit: len(raw) + 20, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	ctx := projectionTestContext()
	if _, err := reader.GetMemory(ctx, firstHash); err != nil {
		test.Fatal(err)
	}
	if _, err := reader.GetMemory(ctx, secondHash); err != nil {
		test.Fatal(err)
	}
	if _, cached := reader.memories[firstHash]; cached {
		test.Fatal("old body retained despite capacity")
	}
	if reader.cacheBytes > reader.cacheLimit {
		test.Fatal("cache weight exceeded bound")
	}
	reads := source.reads
	if !reader.memoryValidated(ctx, firstHash) || source.reads != reads {
		test.Fatal("request-local validation required the evicted body")
	}
	source.memories[firstHash] = domain.MemoryDigest{SnapshotID: source.snapshots[0].ID, Summary: "corrupt reload"}
	if _, err := reader.GetMemory(ctx, firstHash); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatalf("reload bypassed validation: %v", err)
	}
}

func TestProjectionCacheCancellationAfterValidation(test *testing.T) {
	source, repo, _ := projectionSource()
	reader := &ProjectionReader{source: source, repoID: repo, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	hash := source.snapshots[0].MemoryHash
	if _, err := reader.GetMemory(projectionTestContext(), hash); err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(projectionTestContext())
	cancel()
	if reader.memoryValidated(ctx, hash) || !errors.Is(reader.err, context.Canceled) {
		test.Fatal("validation memo ignored cancellation")
	}
}

func TestProjectMemoryCumulativeReadVolumeDoesNotBoundComposition(test *testing.T) {
	repo := projectionTestHash("cumulative-volume-repo")
	source := &projectionTestSource{memories: map[domain.ContentHash]domain.MemoryDigest{}}
	base := projectionTestHash("shared large baseline")
	digest := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: base, Summary: strings.Repeat("B", 512<<10)})
	hash, _ := domain.MemoryDigestHash(digest)
	source.memories[hash] = digest
	source.snapshots = append(source.snapshots, domain.Snapshot{ID: base, RepoID: repo, MemoryHash: hash})
	sequence := []domain.MemoryDigest{digest}
	previous := base
	for index := 0; index < 72; index++ {
		id := projectionTestHash(fmt.Sprint("carry snapshot", index))
		own := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: id, Summary: fmt.Sprint("own contribution ", index), OpenTasks: []string{fmt.Sprint("task ", index)}})
		carried := domain.MergeDigests(digest, own)
		hash, _ := domain.MemoryDigestHash(carried)
		source.memories[hash] = carried
		source.snapshots = append(source.snapshots, domain.Snapshot{ID: id, RepoID: repo, Parents: []domain.ContentHash{previous}, MemoryHash: hash})
		sequence = append(sequence, own)
		previous = id
	}
	got, err := ProjectMemory(projectionTestContext(), source, repo, previous)
	if err != nil {
		test.Fatal(err)
	}
	want := domain.MergeDigestSequence(sequence...)
	want.SnapshotID = previous
	if !reflect.DeepEqual(got.Digest, want) {
		test.Fatal("provenance, order or task composition changed")
	}
	if len(got.Digest.Fragments) != 73 {
		test.Fatalf("fragments=%d", len(got.Digest.Fragments))
	}

	reader := &ProjectionReader{source: source, repoID: repo, cacheLimit: 4096, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	before, err := reader.readState(projectionTestContext(), previous)
	if err != nil {
		test.Fatal(err)
	}
	other, found, complete, err := ProjectLineage(projectionTestContext(), reader, previous)
	if err != nil || !found || !complete || reader.err != nil {
		test.Fatalf("small cache: %v / %v", err, reader.err)
	}
	other.SnapshotID = previous
	other.PreviousMemoryHash = ""
	other.GraftCoverage = nil
	if !reflect.DeepEqual(other, want) || before != got.StateHash {
		test.Fatal("cache capacity changed public projection")
	}
	if reader.bytes <= MaxProjectionMemoryBytes {
		test.Fatalf("fixture did not exceed old read budget: %d", reader.bytes)
	}
	if reader.cacheBytes > reader.cacheLimit {
		test.Fatalf("cache retained %d", reader.cacheBytes)
	}
}

func TestProjectionWalkRejectsUnboundedFrameFanout(test *testing.T) {
	source, repo, tip := projectionSource()
	source.snapshots[1].MemoryHash = ""
	source.snapshots[1].Parents = make([]domain.ContentHash, MaxProjectionWalkFrames+1)
	for index := range source.snapshots[1].Parents {
		source.snapshots[1].Parents[index] = source.snapshots[0].ID
	}
	reader := &ProjectionReader{source: source, repoID: repo, snapshots: map[domain.ContentHash]domain.Snapshot{tip: source.snapshots[1]}, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	_, _, complete, err := ProjectLineage(projectionTestContext(), reader, tip)
	if complete || !errors.Is(err, domain.ErrMemoryProjectionLimit) {
		test.Fatalf("fanout: %v", err)
	}
}

func TestProjectMemoryBoundsRawEdgesBeforeFingerprintOrCoverage(test *testing.T) {
	for _, kind := range []string{"natural", "graft", "cumulative", "valid covering memory"} {
		test.Run(kind, func(test *testing.T) {
			source, repo, tip := projectionSource()
			base := source.snapshots[0].ID
			repeated := func(id domain.ContentHash, count int) []domain.ContentHash {
				out := make([]domain.ContentHash, count)
				for index := range out {
					out[index] = id
				}
				return out
			}
			if kind == "graft" {
				source.snapshots[1].GraftParents = repeated(base, MaxProjectionEdges+1)
			} else if kind == "cumulative" {
				leaf := projectionTestHash("raw edge leaf")
				source.snapshots = append(source.snapshots, domain.Snapshot{ID: leaf, RepoID: repo})
				source.snapshots[0].Parents = repeated(leaf, MaxProjectionEdges/2+1)
				source.snapshots[1].Parents = repeated(base, MaxProjectionEdges/2+1)
			} else {
				source.snapshots[1].Parents = repeated(base, MaxProjectionEdges+1)
			}
			if kind == "valid covering memory" {
				reader := &ProjectionReader{repoID: repo, snapshots: map[domain.ContentHash]domain.Snapshot{}}
				for _, snap := range source.snapshots {
					reader.snapshots[snap.ID] = snap
				}
				fingerprint, ok := newMemoryProjectionFingerprinter(projectionTestContext(), reader).root(source.snapshots[1])
				if !ok {
					test.Fatal("covering fixture must have a valid fingerprint")
				}
				digest := domain.MergeDigests(source.memories[source.snapshots[0].MemoryHash], source.memories[source.snapshots[1].MemoryHash])
				digest.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: domain.MemoryProjectionVersion, ProjectionComplete: true, LineageFingerprint: fingerprint}
				hash, err := domain.MemoryDigestHash(digest)
				if err != nil {
					test.Fatal(err)
				}
				source.memories[hash], source.snapshots[1].MemoryHash = digest, hash
			}
			got, err := ProjectMemory(projectionTestContext(), source, repo, tip)
			if !errors.Is(err, domain.ErrMemoryProjectionLimit) || !reflect.DeepEqual(got, domain.MemoryProjection{}) || source.reads != 0 {
				test.Fatalf("topology must fail before loading memory: err=%v reads=%d", err, source.reads)
			}
		})
	}
}
