package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// This source only serves synthetic, hash-addressed fixture objects. The hook
// observes actual source reads, so a cache hit cannot accidentally satisfy a
// reload-failure test.
type evictionProjectionSource struct {
	*projectionTestSource
	byHash map[domain.ContentHash]int
	before func(context.Context, domain.ContentHash, int) error
}

func (s *evictionProjectionSource) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := ctx.Err(); err != nil {
		return domain.MemoryDigest{}, err
	}
	s.byHash[hash]++
	if s.before != nil {
		if err := s.before(ctx, hash, s.byHash[hash]); err != nil {
			return domain.MemoryDigest{}, err
		}
	}
	return s.projectionTestSource.GetMemory(ctx, repo, hash)
}

func evictionCacheCapacity(t *testing.T, s *projectionTestSource) int {
	t.Helper()
	capacity := 0
	for _, d := range s.memories {
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > capacity {
			capacity = len(raw)
		}
	}
	return capacity // Each object fits; competing bodies cause real eviction.
}

func TestMemoryProjectionEvictedPostorderReloadFailsClosed(t *testing.T) {
	storageFailure := errors.New("synthetic reload unavailable")
	for _, tc := range []struct {
		kind, failure string
	}{
		{"contribution", "storage"}, {"contribution", "corrupt"}, {"contribution", "cancel"},
		{"opaque", "storage"}, {"opaque", "corrupt"}, {"opaque", "cancel"},
	} {
		t.Run(tc.kind+"/"+tc.failure, func(t *testing.T) {
			failure := tc.failure
			base, repo, tip := projectionSource()
			base.snapshots[1].Parents = []domain.ContentHash{base.snapshots[0].ID}
			if tc.kind == "opaque" {
				// A legacy node descends through grafts; its natural parents are
				// scanned only for supplements and need not load a competing body.
				base.snapshots[1].Parents = nil
				base.snapshots[1].GraftParents = []domain.ContentHash{base.snapshots[0].ID}
				opaque := domain.MemoryDigest{SnapshotID: tip, Summary: "opaque descendant"}
				hash, err := domain.MemoryDigestHash(opaque)
				if err != nil {
					t.Fatal(err)
				}
				base.memories = map[domain.ContentHash]domain.MemoryDigest{base.snapshots[0].MemoryHash: base.memories[base.snapshots[0].MemoryHash], hash: opaque}
				base.snapshots[1].MemoryHash = hash
			}
			tipHash := base.snapshots[1].MemoryHash
			parentHash := base.snapshots[0].MemoryHash
			source := &evictionProjectionSource{projectionTestSource: base, byHash: map[domain.ContentHash]int{}}
			reader := &projectionReader{source: source, repoID: repo, cacheLimit: evictionCacheCapacity(t, base)}
			ctx, cancel := context.WithCancel(systemTestContext())
			defer cancel()
			if _, err := reader.readState(ctx, tip); err != nil {
				t.Fatal(err)
			}
			injected := false
			source.before = func(ctx context.Context, hash domain.ContentHash, read int) error {
				if hash != tipHash || read != 2 {
					return nil
				}
				injected = true
				if !reader.validated[tipHash] {
					t.Fatal("reload occurred without prior validation")
				}
				if _, cached := reader.memories[tipHash]; cached {
					t.Fatal("postorder body was not evicted")
				}
				if source.byHash[parentHash] != 1 {
					t.Fatal("reload happened before the parent contribution was read")
				}
				switch failure {
				case "storage":
					return storageFailure
				case "corrupt":
					base.memories[hash] = domain.MemoryDigest{SnapshotID: tip, Summary: "tampered reload"}
				case "cancel":
					cancel() // Cancellation occurs inside the second source lookup.
					return ctx.Err()
				}
				return nil
			}
			got, found, complete, err := memoryProjectionFromDetailed(ctx, reader, tip)
			wantErr := storageFailure
			if failure == "corrupt" {
				wantErr = domain.ErrIntegrity
			} else if failure == "cancel" {
				wantErr = context.Canceled
			}
			if !injected || source.byHash[tipHash] != 2 {
				t.Fatal("fixture did not exercise an evicted postorder contribution reload")
			}
			if !found || complete || !errors.Is(err, wantErr) || !errors.Is(reader.err, wantErr) {
				t.Fatalf("partial parent must not become success: found=%v complete=%v walk=%v reader=%v", found, complete, err, reader.err)
			}
			if !reflect.DeepEqual(got, domain.MemoryDigest{}) {
				t.Fatal("failed walk flushed a partial parent digest")
			}
		})
	}
}

type evictionFixture struct {
	source *projectionTestSource
	repo   domain.ContentHash
}

func newEvictionFixture() *evictionFixture {
	return &evictionFixture{source: &projectionTestSource{memories: map[domain.ContentHash]domain.MemoryDigest{}}, repo: hh("eviction parity repository")}
}

func (f *evictionFixture) add(t *testing.T, name string, parents, grafts []domain.ContentHash, d *domain.MemoryDigest) domain.ContentHash {
	t.Helper()
	id := hh("eviction parity " + name)
	snap := domain.Snapshot{ID: id, RepoID: f.repo, Parents: parents, GraftParents: grafts}
	if len(grafts) > 0 {
		snap.GraftSeq = 1
	}
	if d != nil {
		d.SnapshotID = id
		hash, err := domain.MemoryDigestHash(*d)
		if err != nil {
			t.Fatal(err)
		}
		snap.MemoryHash = hash
		f.source.memories[hash] = *d
	}
	f.source.snapshots = append(f.source.snapshots, snap)
	return id
}

func evictionOwn(name, summary string, facts, tasks []string, authority bool) domain.MemoryDigest {
	id := hh("eviction parity " + name)
	return domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{
		SnapshotID: id, Provider: domain.ProviderCodex,
		Fragments: []domain.MemoryFragment{{SourceSnapshot: id, Summary: summary, KeyFacts: facts, OpenTasks: tasks, TasksAuthoritative: authority}},
	})
}

func assertEvictionProjectionParity(t *testing.T, f *evictionFixture, root domain.ContentHash, want domain.MemoryDigest) {
	t.Helper()
	want.SnapshotID, want.PreviousMemoryHash, want.GraftCoverage = root, "", nil
	standard, err := ProjectMemory(systemTestContext(), f.source, f.repo, root)
	if err != nil {
		t.Fatal(err)
	}
	if !standard.Found || !reflect.DeepEqual(standard.Digest, want) {
		t.Fatalf("default cache changed ordered legacy/provenance semantics\ngot: %#v\nwant: %#v", standard.Digest, want)
	}
	source := &evictionProjectionSource{projectionTestSource: f.source, byHash: map[domain.ContentHash]int{}}
	reader := &projectionReader{source: source, repoID: f.repo, cacheLimit: evictionCacheCapacity(t, f.source)}
	ctx := systemTestContext()
	before, err := reader.readState(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	got, found, complete, err := memoryProjectionFromDetailed(ctx, reader, root)
	if err != nil || reader.err != nil || !found || !complete {
		t.Fatalf("tiny cache: found=%v complete=%v walk=%v reader=%v", found, complete, err, reader.err)
	}
	after, err := reader.readState(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	got.SnapshotID, got.PreviousMemoryHash, got.GraftCoverage = root, "", nil
	if before != after || after != standard.StateHash || !reflect.DeepEqual(got, standard.Digest) {
		t.Fatal("cache eviction changed the full digest or dependency fingerprint")
	}
	tinyJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	standardJSON, err := json.Marshal(standard.Digest)
	if err != nil {
		t.Fatal(err)
	}
	tinyHash, err := domain.MemoryDigestHash(got)
	if err != nil {
		t.Fatal(err)
	}
	standardHash, err := domain.MemoryDigestHash(standard.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tinyJSON, standardJSON) || tinyHash != standardHash {
		t.Fatal("eviction changed exact wire bytes or the memory content hash")
	}
	reloaded := false
	for _, reads := range source.byHash {
		reloaded = reloaded || reads > 1
	}
	if !reloaded || reader.cacheBytes > reader.cacheLimit {
		t.Fatal("fixture did not exercise bounded cache eviction and reload")
	}
}

func TestMemoryProjectionEvictionPreservesNestedLegacyAndMultipleRoots(t *testing.T) {
	for _, withTail := range []bool{false, true} {
		name := "nested supplements and opaque containment"
		if withTail {
			name = "multiple roots pinned provenance and authoritative tasks"
		}
		t.Run(name, func(t *testing.T) {
			f := newEvictionFixture()
			inner := evictionOwn("inner graft", "inner decision", []string{"shared", "inner fact"}, []string{"obsolete inner task"}, false)
			innerID := f.add(t, "inner graft", nil, nil, &inner)
			outer := evictionOwn("outer graft", "outer decision", []string{"outer fact", "shared"}, []string{"outer current"}, true)
			outerID := f.add(t, "outer graft", nil, nil, &outer)
			direct := evictionOwn("direct graft", "direct decision", []string{"direct fact"}, []string{"direct current"}, false)
			directID := f.add(t, "direct graft", nil, nil, &direct)
			ignored := domain.MemoryDigest{Summary: "natural archive must not be appended"}
			origin := f.add(t, "natural origin", nil, nil, &ignored)
			innerBoundary := f.add(t, "inner boundary", []domain.ContentHash{origin}, []domain.ContentHash{innerID}, nil)
			outerBoundary := f.add(t, "outer boundary", []domain.ContentHash{innerBoundary}, []domain.ContentHash{outerID}, nil)
			opaque := domain.MemoryDigest{Provider: domain.ProviderCodex, Summary: "inner decision; outer decision; direct decision; opaque conclusion", KeyFacts: []string{"opaque fact", "shared"}, OpenTasks: []string{"opaque current"}}
			opaqueID := f.add(t, "opaque descendant", []domain.ContentHash{outerBoundary}, []domain.ContentHash{directID}, &opaque)
			// Pin the old containment boundary explicitly. It replaces the rendered
			// supplement fragments but preserves their exact ordered facts/tasks.
			enriched := opaque
			enriched.KeyFacts = []string{"shared", "inner fact", "outer fact", "direct fact", "opaque fact"}
			enriched.OpenTasks = []string{"outer current", "direct current", "opaque current"}
			roots := []domain.ContentHash{opaqueID}
			want := enriched
			if withTail {
				pin := domain.MemoryFragment{SourceSnapshot: hh("eviction parity imported"), Summary: "pinned import", Claims: []domain.MemoryClaim{{Kind: "rationale", Text: "keep pinned reason"}}}
				own := domain.MemoryFragment{SourceSnapshot: hh("eviction parity tail"), Summary: "tail decision", KeyFacts: []string{"tail fact"}, OpenTasks: []string{"tail current"}, TasksAuthoritative: true}
				tail := domain.MemoryDigest{Provider: domain.ProviderCodex, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{
					{SourceSnapshot: hh("eviction parity removed"), Summary: "removed graft must not survive"}, pin, own,
				}, GraftCoverage: &domain.MemoryGraftCoverage{ProjectionVersion: domain.MemoryProjectionVersion, PinnedSources: []domain.ContentHash{pin.SourceSnapshot}}}
				tailID := f.add(t, "tail", []domain.ContentHash{opaqueID}, []domain.ContentHash{innerID}, &tail)
				last := evictionOwn("last root", "last decision", []string{"last fact"}, []string{"last task"}, false)
				lastID := f.add(t, "last root", nil, nil, &last)
				roots = append(roots, tailID, lastID, innerID) // Repeated frontier must stay deduplicated.
				retainedTail := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: tailID, Provider: domain.ProviderCodex, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{pin, own}})
				want = domain.MergeDigestSequence(enriched, retainedTail, last)
				if !reflect.DeepEqual(want.OpenTasks, []string{"tail current", "last task"}) {
					t.Fatal("fixture did not pin authoritative task replacement followed by root append")
				}
			}
			root := f.add(t, "virtual root", roots, nil, nil)
			assertEvictionProjectionParity(t, f, root, want)
		})
	}
}

func TestMemoryProjectionEvictionPreservesRootOrderAndTaskAuthority(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "left then right"
		if reverse {
			name = "right then left"
		}
		t.Run(name, func(t *testing.T) {
			f := newEvictionFixture()
			origin := evictionOwn("ordered origin", "shared origin", []string{"origin fact"}, []string{"obsolete task"}, false)
			originID := f.add(t, "ordered origin", nil, nil, &origin)
			left := evictionOwn("left", "left decision", []string{"left fact"}, []string{"left current"}, true)
			leftID := f.add(t, "left", []domain.ContentHash{originID}, nil, &left)
			right := evictionOwn("right", "right decision", []string{"right fact"}, []string{"right current"}, true)
			rightID := f.add(t, "right", []domain.ContentHash{originID}, nil, &right)
			roots, sequence := []domain.ContentHash{leftID, rightID}, []domain.MemoryDigest{origin, left, right}
			if reverse {
				roots, sequence = []domain.ContentHash{rightID, leftID}, []domain.MemoryDigest{origin, right, left}
			}
			root := f.add(t, "ordered virtual root", roots, nil, nil)
			assertEvictionProjectionParity(t, f, root, domain.MergeDigestSequence(sequence...))
		})
	}
}
