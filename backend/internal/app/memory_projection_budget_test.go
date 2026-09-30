package app

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

func TestProjectionCacheEvictsAndRevalidates(t *testing.T) {
	s, repo, _ := projectionSource()
	a, b := s.snapshots[0].MemoryHash, s.snapshots[1].MemoryHash
	raw, _ := json.Marshal(s.memories[a])
	r := &projectionReader{source: s, repoID: repo, cacheLimit: len(raw) + 20, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	ctx := systemTestContext()
	if _, err := r.GetMemory(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetMemory(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, cached := r.memories[a]; cached {
		t.Fatal("old body retained despite capacity")
	}
	if r.cacheBytes > r.cacheLimit {
		t.Fatal("cache weight exceeded bound")
	}
	reads := s.reads
	if !r.memoryValidated(ctx, a) || s.reads != reads {
		t.Fatal("request-local validation required the evicted body")
	}
	s.memories[a] = domain.MemoryDigest{SnapshotID: s.snapshots[0].ID, Summary: "corrupt reload"}
	if _, err := r.GetMemory(ctx, a); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("reload bypassed validation: %v", err)
	}
}

func TestProjectionCacheCancellationAfterValidation(t *testing.T) {
	s, repo, _ := projectionSource()
	r := &projectionReader{source: s, repoID: repo, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	h := s.snapshots[0].MemoryHash
	if _, err := r.GetMemory(systemTestContext(), h); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(systemTestContext())
	cancel()
	if r.memoryValidated(ctx, h) || !errors.Is(r.err, context.Canceled) {
		t.Fatal("validation memo ignored cancellation")
	}
}

func TestProjectMemoryCumulativeReadVolumeDoesNotBoundComposition(t *testing.T) {
	repo := hh("cumulative-volume-repo")
	s := &projectionTestSource{memories: map[domain.ContentHash]domain.MemoryDigest{}}
	base := hh("shared large baseline")
	d := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: base, Summary: strings.Repeat("B", 512<<10)})
	h, _ := domain.MemoryDigestHash(d)
	s.memories[h] = d
	s.snapshots = append(s.snapshots, domain.Snapshot{ID: base, RepoID: repo, MemoryHash: h})
	sequence := []domain.MemoryDigest{d}
	previous := base
	for i := 0; i < 72; i++ {
		id := hh(fmt.Sprint("carry snapshot", i))
		own := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: id, Summary: fmt.Sprint("own contribution ", i), OpenTasks: []string{fmt.Sprint("task ", i)}})
		carried := domain.MergeDigests(d, own)
		hash, _ := domain.MemoryDigestHash(carried)
		s.memories[hash] = carried
		s.snapshots = append(s.snapshots, domain.Snapshot{ID: id, RepoID: repo, Parents: []domain.ContentHash{previous}, MemoryHash: hash})
		sequence = append(sequence, own)
		previous = id
	}
	got, err := ProjectMemory(systemTestContext(), s, repo, previous)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.MergeDigestSequence(sequence...)
	want.SnapshotID = previous
	if !reflect.DeepEqual(got.Digest, want) {
		t.Fatal("provenance, order or task composition changed")
	}
	if len(got.Digest.Fragments) != 73 {
		t.Fatalf("fragments=%d", len(got.Digest.Fragments))
	}
	// The same selected data also works with a cache too small for any carried
	// archive. This compares the full digest and coherent dependency hash.
	r := &projectionReader{source: s, repoID: repo, cacheLimit: 4096, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	before, err := r.readState(systemTestContext(), previous)
	if err != nil {
		t.Fatal(err)
	}
	other, found, complete, err := memoryProjectionFromDetailed(systemTestContext(), r, previous)
	if err != nil || !found || !complete || r.err != nil {
		t.Fatalf("small cache: %v / %v", err, r.err)
	}
	other.SnapshotID = previous
	other.PreviousMemoryHash = ""
	other.GraftCoverage = nil
	if !reflect.DeepEqual(other, want) || before != got.StateHash {
		t.Fatal("cache capacity changed public projection")
	}
	if r.bytes <= maxProjectionMemoryBytes {
		t.Fatalf("fixture did not exceed old read budget: %d", r.bytes)
	}
	if r.cacheBytes > r.cacheLimit {
		t.Fatalf("cache retained %d", r.cacheBytes)
	}
}

func TestProjectionWalkRejectsUnboundedFrameFanout(t *testing.T) {
	s, repo, tip := projectionSource()
	s.snapshots[1].MemoryHash = ""
	s.snapshots[1].Parents = make([]domain.ContentHash, maxProjectionWalkFrames+1)
	for i := range s.snapshots[1].Parents {
		s.snapshots[1].Parents[i] = s.snapshots[0].ID
	}
	r := &projectionReader{source: s, repoID: repo, snapshots: map[domain.ContentHash]domain.Snapshot{tip: s.snapshots[1]}, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	_, _, complete, err := memoryProjectionFromDetailed(systemTestContext(), r, tip)
	if complete || !errors.Is(err, domain.ErrMemoryProjectionLimit) {
		t.Fatalf("fanout: %v", err)
	}
}

func TestProjectMemoryBoundsRawEdgesBeforeFingerprintOrCoverage(t *testing.T) {
	for _, kind := range []string{"natural", "graft", "cumulative", "valid covering memory"} {
		t.Run(kind, func(t *testing.T) {
			s, repo, tip := projectionSource()
			base := s.snapshots[0].ID
			repeated := func(id domain.ContentHash, n int) []domain.ContentHash {
				out := make([]domain.ContentHash, n)
				for i := range out {
					out[i] = id
				}
				return out
			}
			if kind == "graft" {
				s.snapshots[1].GraftParents = repeated(base, maxProjectionEdges+1)
			} else if kind == "cumulative" {
				leaf := hh("raw edge leaf")
				s.snapshots = append(s.snapshots, domain.Snapshot{ID: leaf, RepoID: repo})
				s.snapshots[0].Parents = repeated(leaf, maxProjectionEdges/2+1)
				s.snapshots[1].Parents = repeated(base, maxProjectionEdges/2+1)
			} else {
				s.snapshots[1].Parents = repeated(base, maxProjectionEdges+1)
			}
			if kind == "valid covering memory" {
				reader := &projectionReader{repoID: repo, snapshots: map[domain.ContentHash]domain.Snapshot{}}
				for _, snap := range s.snapshots {
					reader.snapshots[snap.ID] = snap
				}
				fp, ok := newMemoryProjectionFingerprinter(systemTestContext(), reader).root(s.snapshots[1])
				if !ok {
					t.Fatal("covering fixture must have a valid fingerprint")
				}
				d := domain.MergeDigests(s.memories[s.snapshots[0].MemoryHash], s.memories[s.snapshots[1].MemoryHash])
				d.GraftCoverage = &domain.MemoryGraftCoverage{ProjectionVersion: domain.MemoryProjectionVersion, ProjectionComplete: true, LineageFingerprint: fp}
				hash, err := domain.MemoryDigestHash(d)
				if err != nil {
					t.Fatal(err)
				}
				s.memories[hash], s.snapshots[1].MemoryHash = d, hash
			}
			got, err := ProjectMemory(systemTestContext(), s, repo, tip)
			if !errors.Is(err, domain.ErrMemoryProjectionLimit) || !reflect.DeepEqual(got, domain.MemoryProjection{}) || s.reads != 0 {
				t.Fatalf("topology must fail before loading memory: err=%v reads=%d", err, s.reads)
			}
		})
	}
}
