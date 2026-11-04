package memoryquery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type evictionProjectionSource struct {
	*projectionTestSource
	byHash map[domain.ContentHash]int
	before func(context.Context, domain.ContentHash, int) error
}

func (source *evictionProjectionSource) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := ctx.Err(); err != nil {
		return domain.MemoryDigest{}, err
	}
	source.byHash[hash]++
	if source.before != nil {
		if err := source.before(ctx, hash, source.byHash[hash]); err != nil {
			return domain.MemoryDigest{}, err
		}
	}
	return source.projectionTestSource.GetMemory(ctx, repo, hash)
}

func evictionCacheCapacity(test *testing.T, source *projectionTestSource) int {
	test.Helper()
	capacity := 0
	for _, digest := range source.memories {
		raw, err := json.Marshal(digest)
		if err != nil {
			test.Fatal(err)
		}
		if len(raw) > capacity {
			capacity = len(raw)
		}
	}
	return capacity
}

func TestMemoryProjectionEvictedPostorderReloadFailsClosed(test *testing.T) {
	storageFailure := errors.New("synthetic reload unavailable")
	for _, scenario := range []struct {
		kind, failure string
	}{
		{"contribution", "storage"}, {"contribution", "corrupt"}, {"contribution", "cancel"},
		{"opaque", "storage"}, {"opaque", "corrupt"}, {"opaque", "cancel"},
	} {
		test.Run(scenario.kind+"/"+scenario.failure, func(test *testing.T) {
			failure := scenario.failure
			base, repo, tip := projectionSource()
			base.snapshots[1].Parents = []domain.ContentHash{base.snapshots[0].ID}
			if scenario.kind == "opaque" {

				base.snapshots[1].Parents = nil
				base.snapshots[1].GraftParents = []domain.ContentHash{base.snapshots[0].ID}
				opaque := domain.MemoryDigest{SnapshotID: tip, Summary: "opaque descendant"}
				hash, err := domain.MemoryDigestHash(opaque)
				if err != nil {
					test.Fatal(err)
				}
				base.memories = map[domain.ContentHash]domain.MemoryDigest{base.snapshots[0].MemoryHash: base.memories[base.snapshots[0].MemoryHash], hash: opaque}
				base.snapshots[1].MemoryHash = hash
			}
			tipHash := base.snapshots[1].MemoryHash
			parentHash := base.snapshots[0].MemoryHash
			source := &evictionProjectionSource{projectionTestSource: base, byHash: map[domain.ContentHash]int{}}
			reader := &ProjectionReader{source: source, repoID: repo, cacheLimit: evictionCacheCapacity(test, base)}
			ctx, cancel := context.WithCancel(projectionTestContext())
			defer cancel()
			if _, err := reader.readState(ctx, tip); err != nil {
				test.Fatal(err)
			}
			injected := false
			source.before = func(ctx context.Context, hash domain.ContentHash, read int) error {
				if hash != tipHash || read != 2 {
					return nil
				}
				injected = true
				if !reader.validated[tipHash] {
					test.Fatal("reload occurred without prior validation")
				}
				if _, cached := reader.memories[tipHash]; cached {
					test.Fatal("postorder body was not evicted")
				}
				if source.byHash[parentHash] != 1 {
					test.Fatal("reload happened before the parent contribution was read")
				}
				switch failure {
				case "storage":
					return storageFailure
				case "corrupt":
					base.memories[hash] = domain.MemoryDigest{SnapshotID: tip, Summary: "tampered reload"}
				case "cancel":
					cancel()
					return ctx.Err()
				}
				return nil
			}
			got, found, complete, err := ProjectLineage(ctx, reader, tip)
			wantErr := storageFailure
			if failure == "corrupt" {
				wantErr = domain.ErrIntegrity
			} else if failure == "cancel" {
				wantErr = context.Canceled
			}
			if !injected || source.byHash[tipHash] != 2 {
				test.Fatal("fixture did not exercise an evicted postorder contribution reload")
			}
			if !found || complete || !errors.Is(err, wantErr) || !errors.Is(reader.err, wantErr) {
				test.Fatalf("partial parent must not become success: found=%v complete=%v walk=%v reader=%v", found, complete, err, reader.err)
			}
			if !reflect.DeepEqual(got, domain.MemoryDigest{}) {
				test.Fatal("failed walk flushed a partial parent digest")
			}
		})
	}
}

type evictionFixture struct {
	source *projectionTestSource
	repo   domain.ContentHash
}

func newEvictionFixture() *evictionFixture {
	return &evictionFixture{source: &projectionTestSource{memories: map[domain.ContentHash]domain.MemoryDigest{}}, repo: projectionTestHash("eviction parity repository")}
}

func (fixture *evictionFixture) add(test *testing.T, name string, parents, grafts []domain.ContentHash, digest *domain.MemoryDigest) domain.ContentHash {
	test.Helper()
	id := projectionTestHash("eviction parity " + name)
	snap := domain.Snapshot{ID: id, RepoID: fixture.repo, Parents: parents, GraftParents: grafts}
	if len(grafts) > 0 {
		snap.GraftSeq = 1
	}
	if digest != nil {
		digest.SnapshotID = id
		hash, err := domain.MemoryDigestHash(*digest)
		if err != nil {
			test.Fatal(err)
		}
		snap.MemoryHash = hash
		fixture.source.memories[hash] = *digest
	}
	fixture.source.snapshots = append(fixture.source.snapshots, snap)
	return id
}

func evictionOwn(name, summary string, facts, tasks []string, authority bool) domain.MemoryDigest {
	id := projectionTestHash("eviction parity " + name)
	return domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{
		SnapshotID: id, Provider: domain.ProviderCodex,
		Fragments: []domain.MemoryFragment{{SourceSnapshot: id, Summary: summary, KeyFacts: facts, OpenTasks: tasks, TasksAuthoritative: authority}},
	})
}

func assertEvictionProjectionParity(test *testing.T, fixture *evictionFixture, root domain.ContentHash, want domain.MemoryDigest) {
	test.Helper()
	want.SnapshotID, want.PreviousMemoryHash, want.GraftCoverage = root, "", nil
	standard, err := ProjectMemory(projectionTestContext(), fixture.source, fixture.repo, root)
	if err != nil {
		test.Fatal(err)
	}
	if !standard.Found || !reflect.DeepEqual(standard.Digest, want) {
		test.Fatalf("default cache changed ordered legacy/provenance semantics\ngot: %#v\nwant: %#v", standard.Digest, want)
	}
	source := &evictionProjectionSource{projectionTestSource: fixture.source, byHash: map[domain.ContentHash]int{}}
	reader := &ProjectionReader{source: source, repoID: fixture.repo, cacheLimit: evictionCacheCapacity(test, fixture.source)}
	ctx := projectionTestContext()
	before, err := reader.readState(ctx, root)
	if err != nil {
		test.Fatal(err)
	}
	got, found, complete, err := ProjectLineage(ctx, reader, root)
	if err != nil || reader.err != nil || !found || !complete {
		test.Fatalf("tiny cache: found=%v complete=%v walk=%v reader=%v", found, complete, err, reader.err)
	}
	after, err := reader.readState(ctx, root)
	if err != nil {
		test.Fatal(err)
	}
	got.SnapshotID, got.PreviousMemoryHash, got.GraftCoverage = root, "", nil
	if before != after || after != standard.StateHash || !reflect.DeepEqual(got, standard.Digest) {
		test.Fatal("cache eviction changed the full digest or dependency fingerprint")
	}
	tinyJSON, err := json.Marshal(got)
	if err != nil {
		test.Fatal(err)
	}
	standardJSON, err := json.Marshal(standard.Digest)
	if err != nil {
		test.Fatal(err)
	}
	tinyHash, err := domain.MemoryDigestHash(got)
	if err != nil {
		test.Fatal(err)
	}
	standardHash, err := domain.MemoryDigestHash(standard.Digest)
	if err != nil {
		test.Fatal(err)
	}
	if !bytes.Equal(tinyJSON, standardJSON) || tinyHash != standardHash {
		test.Fatal("eviction changed exact wire bytes or the memory content hash")
	}
	reloaded := false
	for _, reads := range source.byHash {
		reloaded = reloaded || reads > 1
	}
	if !reloaded || reader.cacheBytes > reader.cacheLimit {
		test.Fatal("fixture did not exercise bounded cache eviction and reload")
	}
}

func TestMemoryProjectionEvictionPreservesNestedLegacyAndMultipleRoots(test *testing.T) {
	for _, withTail := range []bool{false, true} {
		name := "nested supplements and opaque containment"
		if withTail {
			name = "multiple roots pinned provenance and authoritative tasks"
		}
		test.Run(name, func(test *testing.T) {
			fixture := newEvictionFixture()
			inner := evictionOwn("inner graft", "inner decision", []string{"shared", "inner fact"}, []string{"obsolete inner task"}, false)
			innerID := fixture.add(test, "inner graft", nil, nil, &inner)
			outer := evictionOwn("outer graft", "outer decision", []string{"outer fact", "shared"}, []string{"outer current"}, true)
			outerID := fixture.add(test, "outer graft", nil, nil, &outer)
			direct := evictionOwn("direct graft", "direct decision", []string{"direct fact"}, []string{"direct current"}, false)
			directID := fixture.add(test, "direct graft", nil, nil, &direct)
			ignored := domain.MemoryDigest{Summary: "natural archive must not be appended"}
			origin := fixture.add(test, "natural origin", nil, nil, &ignored)
			innerBoundary := fixture.add(test, "inner boundary", []domain.ContentHash{origin}, []domain.ContentHash{innerID}, nil)
			outerBoundary := fixture.add(test, "outer boundary", []domain.ContentHash{innerBoundary}, []domain.ContentHash{outerID}, nil)
			opaque := domain.MemoryDigest{Provider: domain.ProviderCodex, Summary: "inner decision; outer decision; direct decision; opaque conclusion", KeyFacts: []string{"opaque fact", "shared"}, OpenTasks: []string{"opaque current"}}
			opaqueID := fixture.add(test, "opaque descendant", []domain.ContentHash{outerBoundary}, []domain.ContentHash{directID}, &opaque)

			enriched := opaque
			enriched.KeyFacts = []string{"shared", "inner fact", "outer fact", "direct fact", "opaque fact"}
			enriched.OpenTasks = []string{"outer current", "direct current", "opaque current"}
			roots := []domain.ContentHash{opaqueID}
			want := enriched
			if withTail {
				pin := domain.MemoryFragment{SourceSnapshot: projectionTestHash("eviction parity imported"), Summary: "pinned import", Claims: []domain.MemoryClaim{{Kind: "rationale", Text: "keep pinned reason"}}}
				own := domain.MemoryFragment{SourceSnapshot: projectionTestHash("eviction parity tail"), Summary: "tail decision", KeyFacts: []string{"tail fact"}, OpenTasks: []string{"tail current"}, TasksAuthoritative: true}
				tail := domain.MemoryDigest{Provider: domain.ProviderCodex, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{
					{SourceSnapshot: projectionTestHash("eviction parity removed"), Summary: "removed graft must not survive"}, pin, own,
				}, GraftCoverage: &domain.MemoryGraftCoverage{ProjectionVersion: domain.MemoryProjectionVersion, PinnedSources: []domain.ContentHash{pin.SourceSnapshot}}}
				tailID := fixture.add(test, "tail", []domain.ContentHash{opaqueID}, []domain.ContentHash{innerID}, &tail)
				last := evictionOwn("last root", "last decision", []string{"last fact"}, []string{"last task"}, false)
				lastID := fixture.add(test, "last root", nil, nil, &last)
				roots = append(roots, tailID, lastID, innerID)
				retainedTail := domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: tailID, Provider: domain.ProviderCodex, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{pin, own}})
				want = domain.MergeDigestSequence(enriched, retainedTail, last)
				if !reflect.DeepEqual(want.OpenTasks, []string{"tail current", "last task"}) {
					test.Fatal("fixture did not pin authoritative task replacement followed by root append")
				}
			}
			root := fixture.add(test, "virtual root", roots, nil, nil)
			assertEvictionProjectionParity(test, fixture, root, want)
		})
	}
}

func TestMemoryProjectionEvictionPreservesRootOrderAndTaskAuthority(test *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "left then right"
		if reverse {
			name = "right then left"
		}
		test.Run(name, func(test *testing.T) {
			fixture := newEvictionFixture()
			origin := evictionOwn("ordered origin", "shared origin", []string{"origin fact"}, []string{"obsolete task"}, false)
			originID := fixture.add(test, "ordered origin", nil, nil, &origin)
			left := evictionOwn("left", "left decision", []string{"left fact"}, []string{"left current"}, true)
			leftID := fixture.add(test, "left", []domain.ContentHash{originID}, nil, &left)
			right := evictionOwn("right", "right decision", []string{"right fact"}, []string{"right current"}, true)
			rightID := fixture.add(test, "right", []domain.ContentHash{originID}, nil, &right)
			roots, sequence := []domain.ContentHash{leftID, rightID}, []domain.MemoryDigest{origin, left, right}
			if reverse {
				roots, sequence = []domain.ContentHash{rightID, leftID}, []domain.MemoryDigest{origin, right, left}
			}
			root := fixture.add(test, "ordered virtual root", roots, nil, nil)
			assertEvictionProjectionParity(test, fixture, root, domain.MergeDigestSequence(sequence...))
		})
	}
}
