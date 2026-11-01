package repositoryquery

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGraphProjectionClosedEncoderOwnsIndependentBuffers(test *testing.T) {
	graph := domain.GraphState{Version: 1, ScopeLabels: map[string]string{"main": strings.Repeat("scope", 8192)}}
	first, err := EncodeGraphProjection(context.Background(), graph)
	if err != nil {
		test.Fatal(err)
	}
	second, err := EncodeGraphProjection(context.Background(), graph)
	if err != nil || len(first) == 0 || !bytes.Equal(first, second) || &first[0] == &second[0] {
		test.Fatal("closed encoders shared or changed their owned buffers", err)
	}
	first[0] ^= 1
	decoded, err := DecodeGraphProjection(context.Background(), second)
	if err != nil || !reflect.DeepEqual(decoded, graph) {
		test.Fatal("caller mutation affected an independent encoded graph", err)
	}
}

func TestGraphCacheRetainedAdmissionAndOwnedBytes(test *testing.T) {
	var cache GraphCache
	var stats struct{ Entries, Bytes, Building int } = cache.Stats()
	if ProjectionRetentionBytes != GraphProjectionCacheBytes {
		test.Fatal("retention policy is not tied to the existing graph envelope")
	}
	if ProjectionRetentionBytes != 8<<20 || cache.RetainedBytes() != 0 || stats.Entries != 0 {
		test.Fatal("zero-value retention or legacy stats contract changed")
	}
	key := GraphProjectionKey{Repo: legacyCacheHash(test.Name()), Position: "HEAD"}
	encoded := make([]byte, 3, 4096)
	copy(encoded, "zip")
	cache.Put(key, encoded)
	borrowed, hit := cache.Get(key)
	if !hit || &borrowed[0] == &encoded[0] || !bytes.Equal(borrowed, encoded) {
		test.Fatal("graph must own the input backing and return a borrowed cache view")
	}
	before := cache.RetainedBytes()
	if before < cap(borrowed) || cap(borrowed) != len(borrowed) || before > ProjectionRetentionBytes || cache.Stats().Bytes != len(encoded) {
		test.Fatal("owned backing is not bounded independently of producer capacity")
	}
	huge := strings.Repeat("k", ProjectionRetentionBytes+1)
	for _, scenario := range []struct {
		name    string
		key     GraphProjectionKey
		encoded []byte
	}{
		{"encoded length", key, make([]byte, GraphProjectionCacheBytes+1)},
		{"position", GraphProjectionKey{Repo: key.Repo, Position: huge}, []byte("x")},
		{"origin", GraphProjectionKey{Repo: key.Repo, Origin: huge}, []byte("x")},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			cache.Put(scenario.key, scenario.encoded)
			current, found := cache.Get(key)
			if !found || &current[0] != &borrowed[0] || cache.RetainedBytes() != before || cache.Stats() != statsForTransferredGraph(len(encoded)) {
				test.Fatal("oversized optional candidate displaced or changed the warm entry")
			}
		})
	}
	_, finish, err := cache.Lookup(GraphProjectionKey{Repo: legacyCacheHash("building only")})
	if err != nil || finish == nil {
		test.Fatal("ordinary build reservation failed", err)
	}
	defer finish()
	if cache.RetainedBytes() != before || cache.Stats().Building != 1 {
		test.Fatal("persistent entry accounting included an active builder")
	}
	producer := make([]byte, ProjectionRetentionBytes+3)
	copy(producer[len(producer)-3:], "zip")
	tail := producer[len(producer)-3:]
	var detached GraphCache
	detached.Put(key, tail)
	stored, found := detached.Get(key)
	if !found || &stored[0] == &tail[0] || cap(stored) != len(stored) || detached.RetainedBytes() > 4096 {
		test.Fatal("tiny suffix retained the producer's oversized allocation")
	}
	tail[0] = 'x'
	if string(stored) != "zip" {
		test.Fatal("producer mutation changed the cached encoding")
	}
}

func statsForTransferredGraph(length int) struct{ Entries, Bytes, Building int } {
	return struct{ Entries, Bytes, Building int }{Entries: 1, Bytes: length}
}

func TestGraphCacheRetainedMetadataCapacityAndOwnedKeys(test *testing.T) {
	var cache GraphCache
	large := strings.Repeat("g", ProjectionRetentionBytes*3/8)
	var keys []GraphProjectionKey
	for index := range 3 {
		key := GraphProjectionKey{Repo: legacyCacheHash(fmt.Sprint("graph", index)), Origin: large}
		keys = append(keys, key)
		cache.Put(key, []byte("x"))
		retained := cache.RetainedBytes()
		stats := cache.Stats()
		if retained < len(large)*stats.Entries || retained > ProjectionRetentionBytes || stats.Bytes != stats.Entries || stats.Entries > GraphProjectionCacheEntries {
			test.Fatal("small-count graph key metadata escaped the retained bound", stats, retained)
		}
	}
	if _, hit := cache.Get(keys[0]); hit {
		test.Fatal("aggregate metadata pressure did not evict the oldest graph")
	}
	backing := strings.Repeat("s", ProjectionRetentionBytes)
	short := backing[:7]
	key := GraphProjectionKey{Repo: legacyCacheHash("owned graph key"), Origin: short, Position: short}
	var owned GraphCache
	owned.Put(key, []byte("x"))
	entry := owned.entries[0]
	if entry.key != key || unsafe.StringData(entry.key.Origin) == unsafe.StringData(short) || unsafe.StringData(entry.key.Position) == unsafe.StringData(short) {
		test.Fatal("graph key retained caller string backing")
	}
}

func TestBranchCacheRetainedLargeMetadataAdmission(test *testing.T) {
	huge := strings.Repeat("b", ProjectionRetentionBytes+1)
	for _, scenario := range []struct {
		name   string
		change func(*BranchProjectionKey, map[string]domain.BranchContext)
	}{
		{"origin", func(key *BranchProjectionKey, value map[string]domain.BranchContext) { key.Origin = huge }},
		{"map key", func(key *BranchProjectionKey, value map[string]domain.BranchContext) {
			value[huge] = domain.BranchContext{}
		}},
		{"branch identity", func(key *BranchProjectionKey, value map[string]domain.BranchContext) {
			value["main"] = domain.BranchContext{BranchID: huge}
		}},
		{"code commit", func(key *BranchProjectionKey, value map[string]domain.BranchContext) {
			value["main"] = domain.BranchContext{CodeCommit: huge}
		}},
		{"reason", func(key *BranchProjectionKey, value map[string]domain.BranchContext) {
			value["main"] = domain.BranchContext{Reason: huge}
		}},
		{"merge metadata", func(key *BranchProjectionKey, value map[string]domain.BranchContext) {
			value["main"] = domain.BranchContext{Merges: []domain.BranchContextMerge{{Reason: huge}}}
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			var cache BranchCache
			repo := legacyCacheHash(test.Name())
			key := BranchProjectionKey{Repo: repo, Generation: legacyCacheGeneration(repo)}
			cache.Put(key, map[string]domain.BranchContext{"main": {Reason: "warm"}})
			before, stats := cache.RetainedBytes(), cache.Stats()
			candidate := key
			candidate.Graph++
			value := map[string]domain.BranchContext{"main": {}}
			scenario.change(&candidate, value)
			cache.Put(candidate, value)
			warm, hit := cache.Get(key)
			if !hit || warm["main"].Reason != "warm" || cache.RetainedBytes() != before || cache.Stats() != stats {
				test.Fatal("oversized branch metadata entered or displaced the warm cache")
			}
		})
	}
}

func TestBranchCacheRetainedCapacityAndStringOwnership(test *testing.T) {
	var cache BranchCache
	var zero struct{ Entries, Weight int } = cache.Stats()
	if zero.Entries != 0 || zero.Weight != 0 || cache.RetainedBytes() != 0 {
		test.Fatal("nonempty zero-value branch cache")
	}
	large := strings.Repeat("b", ProjectionRetentionBytes*3/8)
	var keys []BranchProjectionKey
	for index := range 3 {
		repo := legacyCacheHash(fmt.Sprint("branch", index))
		key := BranchProjectionKey{Repo: repo, Generation: legacyCacheGeneration(repo)}
		keys = append(keys, key)
		cache.Put(key, map[string]domain.BranchContext{"main": {Reason: large}})
		stats, retained := cache.Stats(), cache.RetainedBytes()
		if retained < len(large)*stats.Entries || retained > ProjectionRetentionBytes || stats.Weight != stats.Entries || stats.Entries > 8 {
			test.Fatal("branch metadata changed legacy weight or escaped the byte bound", stats, retained)
		}
	}
	if _, hit := cache.Get(keys[0]); hit {
		test.Fatal("aggregate metadata pressure did not evict the oldest branch")
	}
	short := strings.Repeat("s", ProjectionRetentionBytes)[:7]
	repo := legacyCacheHash("owned branch strings")
	key := BranchProjectionKey{Repo: repo, Origin: short, Generation: legacyCacheGeneration(repo)}
	var owned BranchCache
	owned.Put(key, map[string]domain.BranchContext{short: {Reason: short, Merges: []domain.BranchContextMerge{{EventID: short}}}})
	entry := owned.entries[0]
	for branch, value := range entry.value {
		for _, actual := range []string{entry.key.Origin, branch, value.Reason, value.Merges[0].EventID} {
			if actual != short || unsafe.StringData(actual) == unsafe.StringData(short) {
				test.Fatal("branch cache retained caller substring backing")
			}
		}
	}
}

func retentionSegmentBasis() contextSegmentBasis {
	snapshot := domain.Snapshot{ID: legacyCacheHash("retention snapshot"), Message: "message", Parents: []domain.ContentHash{legacyCacheHash("parent")}, GraftParents: []domain.ContentHash{legacyCacheHash("graft")}, Branches: []string{"main"}, Models: []string{"model"}}
	return contextSegmentBasis{
		view: domain.ContextQueryView{
			Branch: "main", Snapshots: []domain.Snapshot{snapshot},
			History:   []domain.HistoryEvent{{ID: "event", Branch: "main", PR: &domain.PullRequestMerge{BaseBranch: "main"}}},
			Semantics: domain.ContextSemantics{Version: 1, Merges: []domain.ContextMergeEvidence{{EventID: "merge", Lineage: "natural"}}},
			Inclusion: &domain.BranchContext{Reason: "included", Roots: []domain.ContentHash{snapshot.ID}, SnapshotIDs: []domain.ContentHash{snapshot.ID}, Merges: []domain.BranchContextMerge{{State: "included"}}},
		},
		selected: map[domain.ContentHash]domain.Snapshot{snapshot.ID: snapshot},
		bindings: map[domain.ContentHash][]domain.CommitContextBinding{snapshot.ID: {{EventID: "publication", SegmentIDs: []domain.ContentHash{legacyCacheHash("segment")}}}},
	}
}

func TestSegmentCacheRetainedFullBasisMetadataAdmission(test *testing.T) {
	huge := strings.Repeat("m", ProjectionRetentionBytes+1)
	for _, scenario := range []struct {
		name   string
		change func(*contextSegmentCacheKey, *contextSegmentBasis)
	}{
		{"key origin", func(key *contextSegmentCacheKey, basis *contextSegmentBasis) { key.origin = huge }},
		{"key branch", func(key *contextSegmentCacheKey, basis *contextSegmentBasis) { key.branch = huge }},
		{"view branch", func(key *contextSegmentCacheKey, basis *contextSegmentBasis) { basis.view.Branch = huge }},
		{"snapshot message", func(key *contextSegmentCacheKey, basis *contextSegmentBasis) { basis.view.Snapshots[0].Message = huge }},
		{"snapshot model", func(key *contextSegmentCacheKey, basis *contextSegmentBasis) {
			basis.view.Snapshots[0].Models[0] = huge
		}},
		{"history", func(key *contextSegmentCacheKey, basis *contextSegmentBasis) { basis.view.History[0].Branch = huge }},
		{"nested history", func(key *contextSegmentCacheKey, basis *contextSegmentBasis) {
			basis.view.History[0].PR.BaseBranch = huge
		}},
		{"semantics", func(key *contextSegmentCacheKey, basis *contextSegmentBasis) {
			basis.view.Semantics.Merges[0].Lineage = huge
		}},
		{"inclusion", func(key *contextSegmentCacheKey, basis *contextSegmentBasis) { basis.view.Inclusion.Reason = huge }},
		{"selected snapshot", func(key *contextSegmentCacheKey, basis *contextSegmentBasis) {
			for snapshotID, snapshot := range basis.selected {
				snapshot.Message = huge
				basis.selected[snapshotID] = snapshot
			}
		}},
		{"binding metadata", func(key *contextSegmentCacheKey, basis *contextSegmentBasis) {
			for snapshotID := range basis.bindings {
				basis.bindings[snapshotID][0].WorktreeID = huge
			}
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			var cache SegmentCache
			key := segmentOwnerCacheKey(legacyCacheHash(test.Name()))
			cache.put(key, retentionSegmentBasis())
			before, stats := cache.RetainedBytes(), cache.Stats()
			candidate, basis := key, retentionSegmentBasis()
			candidate.source.Sequence++
			scenario.change(&candidate, &basis)
			cache.put(candidate, basis)
			if _, hit := cache.get(candidate); hit {
				test.Fatal("small-count oversized full-basis metadata entered the cache")
			}
			warm, hit := cache.get(key)
			if !hit || len(warm.view.History) != 1 || warm.view.Semantics.Merges[0].Lineage != "natural" || cache.RetainedBytes() != before || cache.Stats() != stats {
				test.Fatal("oversized optional basis changed the warm full basis")
			}
		})
	}
}

func TestSegmentCacheRetainedCapacityDuplicateAndOwnedStrings(test *testing.T) {
	var cache SegmentCache
	if cache.RetainedBytes() != 0 || cache.Stats() != (SegmentCacheStats{}) {
		test.Fatal("nonempty zero-value segment cache")
	}
	key := segmentOwnerCacheKey(legacyCacheHash(test.Name()))
	first := key
	large := strings.Repeat("h", ProjectionRetentionBytes*3/8)
	for sequence := range 3 {
		key.source.Sequence += int64(sequence)
		basis := contextSegmentBasis{view: domain.ContextQueryView{History: []domain.HistoryEvent{{Branch: large}}}}
		cache.put(key, basis)
		stats, retained := cache.Stats(), cache.RetainedBytes()
		if retained < len(large)*stats.Entries || retained > ProjectionRetentionBytes || stats.Weight != stats.Entries || stats.Entries > 8 {
			test.Fatal("full history was pruned or escaped the retained bound", stats, retained)
		}
		cached, hit := cache.get(key)
		if !hit || len(cached.view.History) != 1 || cached.view.History[0].Branch != large {
			test.Fatal("admitted basis lost full history")
		}
	}
	if _, hit := cache.get(first); hit {
		test.Fatal("aggregate metadata pressure did not evict the oldest segment")
	}
	before, stats := cache.RetainedBytes(), cache.Stats()
	cache.put(key, contextSegmentBasis{})
	if cache.RetainedBytes() != before || cache.Stats() != stats {
		test.Fatal("same-source duplicate replaced immutable basis or inflated bytes")
	}
	short := strings.Repeat("s", ProjectionRetentionBytes)[:7]
	key.origin, key.branch = short, short
	basis := retentionSegmentBasis()
	basis.view.History[0].Branch = short
	var owned SegmentCache
	owned.put(key, basis)
	entry := owned.entries[0]
	for _, actual := range []string{entry.key.origin, entry.key.branch, entry.value.view.History[0].Branch} {
		if actual != short || unsafe.StringData(actual) == unsafe.StringData(short) {
			test.Fatal("segment cache retained caller substring backing")
		}
	}
}

func mutateRetentionSegmentBasis(basis contextSegmentBasis) {
	basis.view.Snapshots[0].Message = "changed"
	basis.view.Snapshots[0].Parents[0] = "changed"
	basis.view.Snapshots[0].GraftParents[0] = "changed"
	basis.view.Snapshots[0].Branches[0] = "changed"
	basis.view.Snapshots[0].Models[0] = "changed"
	basis.view.History[0].Branch = "changed"
	basis.view.History[0].PR.BaseBranch = "changed"
	basis.view.Semantics.Merges[0].Lineage = "changed"
	basis.view.Inclusion.Roots[0] = "changed"
	basis.view.Inclusion.SnapshotIDs[0] = "changed"
	basis.view.Inclusion.Merges[0].State = "changed"
	for snapshotID, snapshot := range basis.selected {
		snapshot.Message = "changed"
		snapshot.Parents[0] = "changed"
		snapshot.Models[0] = "changed"
		basis.selected[snapshotID] = snapshot
	}
	for snapshotID := range basis.bindings {
		basis.bindings[snapshotID][0].EventID = "changed"
		basis.bindings[snapshotID][0].SegmentIDs[0] = "changed"
		delete(basis.bindings, snapshotID)
	}
}

func TestSegmentCacheRetainedProducerAndNestedOutputIsolation(test *testing.T) {
	var cache SegmentCache
	key := segmentOwnerCacheKey(legacyCacheHash(test.Name()))
	input := retentionSegmentBasis()
	cache.put(key, input)
	before := cache.RetainedBytes()
	mutateRetentionSegmentBasis(input)
	actual, hit := cache.get(key)
	if !hit || !reflect.DeepEqual(actual, retentionSegmentBasis()) {
		test.Fatal("cached full basis aliases producer containers or nested values")
	}
	again, hit := cache.get(key)
	if !hit || !reflect.DeepEqual(again, retentionSegmentBasis()) || cache.RetainedBytes() != before || &again.view.Snapshots[0] != &actual.view.Snapshots[0] {
		test.Fatal("warm private reads copied or changed the immutable basis")
	}
}

func TestProjectionCachesRetainedRemovalClearsBackingSlots(test *testing.T) {
	test.Run("graph same repository replacement", func(test *testing.T) {
		var cache GraphCache
		key := GraphProjectionKey{Repo: legacyCacheHash(test.Name()), Position: "first"}
		cache.Put(key, []byte("first"))
		other := key
		other.Position = "second"
		cache.Put(other, []byte("second"))
		slots := cache.entries[:cap(cache.entries)]
		key.Revision.Graph++
		cache.Put(key, []byte("next"))
		var fresh GraphCache
		fresh.Put(key, []byte("next"))
		if cache.RetainedBytes() != fresh.RetainedBytes() || cache.Stats().Entries != 1 {
			test.Fatal("graph replacement leaked accounting")
		}
		for slot := range slots {
			live := false
			for index := range cache.entries {
				live = live || &slots[slot] == &cache.entries[index]
			}
			if !live && !reflect.ValueOf(slots[slot]).IsZero() {
				test.Fatal("removed graph slot retains key or byte backing")
			}
		}
	})
	test.Run("branch FIFO", func(test *testing.T) {
		var cache BranchCache
		var first BranchProjectionKey
		for index := range 8 {
			repo := legacyCacheHash(fmt.Sprint("slot", index))
			key := BranchProjectionKey{Repo: repo, Generation: legacyCacheGeneration(repo)}
			if index == 0 {
				first = key
			}
			cache.Put(key, map[string]domain.BranchContext{"main": {Reason: "retained"}})
		}
		slots := cache.entries[:cap(cache.entries)]
		repo := legacyCacheHash("ninth branch")
		cache.Put(BranchProjectionKey{Repo: repo, Generation: legacyCacheGeneration(repo)}, map[string]domain.BranchContext{"main": {}})
		if _, hit := cache.Get(first); hit || cache.Stats().Entries != 8 {
			test.Fatal("legacy branch FIFO bound changed")
		}
		for slot := range slots {
			live := false
			for index := range cache.entries {
				live = live || &slots[slot] == &cache.entries[index]
			}
			if !live && !reflect.ValueOf(slots[slot]).IsZero() {
				test.Fatal("removed branch slot retains key or map backing")
			}
		}
	})
	test.Run("segment FIFO", func(test *testing.T) {
		var cache SegmentCache
		key := segmentOwnerCacheKey(legacyCacheHash(test.Name()))
		first := key
		for sequence := range 8 {
			key.source.Sequence = first.source.Sequence + int64(sequence)
			cache.put(key, retentionSegmentBasis())
		}
		slots := cache.entries[:cap(cache.entries)]
		key.source.Sequence++
		cache.put(key, retentionSegmentBasis())
		if _, hit := cache.get(first); hit || cache.Stats() != (SegmentCacheStats{Entries: 8, Weight: 16}) {
			test.Fatal("legacy segment FIFO weight contract changed")
		}
		for slot := range slots {
			live := false
			for index := range cache.entries {
				live = live || &slots[slot] == &cache.entries[index]
			}
			if !live && !reflect.ValueOf(slots[slot]).IsZero() {
				test.Fatal("removed segment slot retains full basis backing")
			}
		}
	})
}

func TestProjectionCachesRetainedConcurrentPutGet(test *testing.T) {
	var graphs GraphCache
	var branches BranchCache
	var segments SegmentCache
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			repo := legacyCacheHash(fmt.Sprint("concurrent retention", worker))
			graphKey := GraphProjectionKey{Repo: repo, Source: legacyCacheGeneration(repo)}
			branchKey := BranchProjectionKey{Repo: repo, Generation: graphKey.Source}
			segmentKey := segmentOwnerCacheKey(repo)
			for range 12 {
				graphs.Put(graphKey, []byte("immutable"))
				encoded, hit := graphs.Get(graphKey)
				if !hit || string(encoded) != "immutable" {
					test.Error("concurrent graph read lost immutable transferred bytes")
					return
				}
				branches.Put(branchKey, map[string]domain.BranchContext{"main": {SnapshotIDs: []domain.ContentHash{repo}}})
				branch, hit := branches.Get(branchKey)
				if !hit || len(branch["main"].SnapshotIDs) != 1 || branch["main"].SnapshotIDs[0] != repo {
					test.Error("concurrent branch read lost owned values")
					return
				}
				branch["main"].SnapshotIDs[0] = "caller mutation"
				segments.put(segmentKey, retentionSegmentBasis())
				basis, hit := segments.get(segmentKey)
				if !hit || !reflect.DeepEqual(basis, retentionSegmentBasis()) {
					test.Error("concurrent segment read lost full owned basis")
					return
				}
				for _, retained := range []int{graphs.RetainedBytes(), branches.RetainedBytes(), segments.RetainedBytes()} {
					if retained <= 0 || retained > ProjectionRetentionBytes {
						test.Error("concurrent persistent retained accounting escaped its bound", retained)
						return
					}
				}
			}
		}()
	}
	workers.Wait()
	if graphs.Stats().Entries != 4 || branches.Stats().Weight != 8 || segments.Stats() != (SegmentCacheStats{Entries: 4, Weight: 8}) {
		test.Fatal("small concurrent cache population changed legacy entry or weight counters")
	}
}
