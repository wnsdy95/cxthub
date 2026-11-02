package repositoryquery

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func legacyCacheHash(value string) domain.ContentHash {
	return domain.HashContent([]byte(value))
}

func legacyCacheGeneration(repo domain.ContentHash) domain.RepositoryViewSourceCheckpoint {
	return domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope, RepoID: repo, Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 7}
}

func TestGraphProjectionCacheBoundsAndBusyRetry(test *testing.T) {
	var cache GraphCache
	first := GraphProjectionKey{Repo: legacyCacheHash("repo")}
	_, finish, err := cache.Lookup(first)
	if err != nil || finish == nil {
		test.Fatalf("first reservation: %v", err)
	}
	if encoded, release, err := cache.Lookup(first); !errors.Is(err, domain.ErrResourceOverloaded) || encoded != nil || release != nil {
		test.Fatal("concurrent identical miss must retry instead of holding a read slot")
	}
	second := GraphProjectionKey{Repo: legacyCacheHash("second")}
	_, finishSecond, err := cache.Lookup(second)
	if err != nil || finishSecond == nil {
		test.Fatalf("second reservation: %v", err)
	}
	if stats := cache.Stats(); stats.Entries != 0 || stats.Bytes != 0 || stats.Building != 2 {
		test.Fatalf("builder accounting: %+v", stats)
	}
	if encoded, release, err := cache.Lookup(GraphProjectionKey{Repo: legacyCacheHash("third")}); !errors.Is(err, domain.ErrResourceOverloaded) || encoded != nil || release != nil {
		test.Fatal("more than two independent builders admitted")
	}
	finish()
	_, finish, err = cache.Lookup(first)
	if err != nil || finish == nil {
		test.Fatalf("released reservation could not retry: %v", err)
	}
	finish()
	finishSecond()
	if stats := cache.Stats(); stats.Building != 0 {
		test.Fatalf("builder reservations leaked: %+v", stats)
	}
	cache.Put(first, make([]byte, GraphProjectionCacheBytes+1))
	if stats := cache.Stats(); stats.Entries != 0 || stats.Bytes != 0 {
		test.Fatal("oversized projection cached")
	}
	for index := range 10 {
		cache.Put(GraphProjectionKey{Repo: legacyCacheHash(fmt.Sprint(index))}, make([]byte, 3<<20))
		if stats := cache.Stats(); stats.Bytes > GraphProjectionCacheBytes || stats.Entries > GraphProjectionCacheEntries {
			test.Fatal("cache exceeded retention bound")
		}
	}
	for index := range 10 {
		cache.Put(GraphProjectionKey{Repo: legacyCacheHash(fmt.Sprint(index))}, []byte("{}"))
	}
	if stats := cache.Stats(); stats.Entries != GraphProjectionCacheEntries {
		test.Fatal("entry count bound not exercised")
	}
}

func TestGraphProjectionCacheIdentityAndOrderedKeyCopies(test *testing.T) {
	var cache GraphCache
	repo := legacyCacheHash(test.Name())
	first := GraphProjectionKey{Repo: repo, Revision: domain.RepositoryRevision{Graph: 1}, Source: legacyCacheGeneration(repo), Inputs: sha256.Sum256([]byte("view")), Origin: "origin"}
	if encoded, found := cache.Get(first); found || encoded != nil || len(cache.Keys()) != 0 {
		test.Fatal("zero-value cache returned data")
	}
	encoded := []byte("retained encoding")
	cache.Put(first, encoded)
	for _, change := range []func(*GraphProjectionKey){
		func(key *GraphProjectionKey) { key.Repo = legacyCacheHash("other") },
		func(key *GraphProjectionKey) { key.Revision.Graph++ },
		func(key *GraphProjectionKey) { key.Revision.Pending++ },
		func(key *GraphProjectionKey) { key.Revision.Evidence++ },
		func(key *GraphProjectionKey) { key.Source.Sequence++ },
		func(key *GraphProjectionKey) { key.Source.Epoch = "22345678-abcd-4321-9876-123456789abc" },
		func(key *GraphProjectionKey) { key.Inputs[0] ^= 1 },
		func(key *GraphProjectionKey) { key.Position = "position" },
		func(key *GraphProjectionKey) { key.Origin = "other-origin" },
	} {
		changed := first
		change(&changed)
		if _, found := cache.Get(changed); found {
			test.Fatalf("cache crossed projection identity: %+v", changed)
		}
	}
	position := first
	position.Position = "position"
	cache.Put(position, []byte("position encoding"))
	keys := cache.Keys()
	if !reflect.DeepEqual(keys, []GraphProjectionKey{first, position}) {
		test.Fatalf("retention order changed: %+v", keys)
	}
	keys[0].Origin = "caller mutation"
	if !reflect.DeepEqual(cache.Keys(), []GraphProjectionKey{first, position}) {
		test.Fatal("key snapshot aliases cache storage")
	}
	view, found := cache.Get(first)
	if !found || !bytes.Equal(view, encoded) || &view[0] == &encoded[0] {
		test.Fatal("encoded cache did not own its input or lost its read-view")
	}
	again, found := cache.Get(first)
	if !found || &again[0] != &view[0] {
		test.Fatal("warm read copied its immutable cached encoding")
	}
	replacement := first
	replacement.Source.Sequence++
	replacement.Inputs[0] ^= 1
	replacement.Origin = "new-origin"
	cache.Put(replacement, []byte("new encoding"))
	if _, found := cache.Get(first); found || !reflect.DeepEqual(cache.Keys(), []GraphProjectionKey{position, replacement}) {
		test.Fatal("same-position replacement retained stale source/input/origin")
	}
	next := replacement
	next.Revision.Pending++
	cache.Put(next, []byte("next revision"))
	if !reflect.DeepEqual(cache.Keys(), []GraphProjectionKey{next}) {
		test.Fatal("revision replacement retained another position")
	}
	if stats := cache.Stats(); stats.Entries != 1 || stats.Bytes != len("next revision") || stats.Building != 0 {
		test.Fatalf("replacement byte accounting: %+v", stats)
	}
	if keys[1] != position {
		test.Fatal("cache writes mutated an earlier key snapshot")
	}
}

func TestGraphProjectionCacheHitAndBuilderIsolation(test *testing.T) {
	var cache GraphCache
	var independent GraphCache
	first := GraphProjectionKey{Repo: legacyCacheHash("first")}
	second := GraphProjectionKey{Repo: legacyCacheHash("second")}
	_, finishFirst, err := cache.Lookup(first)
	if err != nil || finishFirst == nil {
		test.Fatalf("first reservation: %v", err)
	}
	defer finishFirst()
	_, finishSecond, err := cache.Lookup(second)
	if err != nil || finishSecond == nil {
		test.Fatalf("second reservation: %v", err)
	}
	defer finishSecond()
	encoded := []byte("cached")
	cache.Put(first, encoded)
	cached, found := cache.Get(first)
	if !found || &cached[0] == &encoded[0] {
		test.Fatal("cache insertion retained the producer backing")
	}
	view, finish, err := cache.Lookup(first)
	if err != nil || finish != nil || len(view) == 0 || &view[0] != &cached[0] || cache.Stats().Building != 2 {
		test.Fatal("cache hit reserved a builder or copied its immutable view")
	}
	if _, found := independent.Get(first); found {
		test.Fatal("separate caches shared retained bytes")
	}
	_, finish, err = independent.Lookup(first)
	if err != nil || finish == nil {
		test.Fatalf("separate caches shared builder admission: %v", err)
	}
	finish()
}

func TestBranchProjectionCacheOwnsInputsAndBoundsMemory(test *testing.T) {
	var cache BranchCache
	key := BranchProjectionKey{Repo: legacyCacheHash("repo"), Graph: 1, Evidence: 2, Generation: legacyCacheGeneration(legacyCacheHash("repo"))}
	value := map[string]domain.BranchContext{"main": {Roots: []domain.ContentHash{legacyCacheHash("root")}, SnapshotIDs: []domain.ContentHash{legacyCacheHash("tip")}, Merges: []domain.BranchContextMerge{{State: "included"}}}}
	expected := CloneBranchContexts(value)
	cache.Put(key, value)
	value["main"].SnapshotIDs[0] = legacyCacheHash("changed")
	value["main"].Roots[0] = legacyCacheHash("changed root")
	value["main"].Merges[0].State = "changed"
	value["caller"] = domain.BranchContext{}
	actual, found := cache.Get(key)
	if !found || !reflect.DeepEqual(actual, expected) {
		test.Fatal("cache aliases input")
	}
	actual["main"].Merges[0].State = "changed"
	actual["main"].Roots[0] = legacyCacheHash("changed result root")
	actual["main"].SnapshotIDs[0] = legacyCacheHash("changed result tip")
	delete(actual, "main")
	again, _ := cache.Get(key)
	if !reflect.DeepEqual(again, expected) {
		test.Fatal("cache aliases output")
	}
	for _, change := range []func(*BranchProjectionKey){
		func(other *BranchProjectionKey) {
			other.Repo = legacyCacheHash("other")
			other.Generation.RepoID = other.Repo
		},
		func(other *BranchProjectionKey) { other.Graph++ },
		func(other *BranchProjectionKey) { other.Evidence++ },
		func(other *BranchProjectionKey) { other.Origin = "new-origin" },
	} {
		other := key
		change(&other)
		if _, found := cache.Get(other); found {
			test.Fatal("cache crossed revision/repository boundary")
		}
	}
	next := key
	next.Graph++
	cache.Put(next, expected)
	if _, found := cache.Get(key); found {
		test.Fatal("old generation retained")
	}
	for index := range 20 {
		repo := legacyCacheHash(fmt.Sprint(index))
		cache.Put(BranchProjectionKey{Repo: repo, Generation: legacyCacheGeneration(repo)}, expected)
	}
	if stats := cache.Stats(); stats.Entries != 8 || stats.Weight > MaxBranchProjectionWeight {
		test.Fatal("cache unbounded")
	}
	cache.Put(key, map[string]domain.BranchContext{"large": {SnapshotIDs: make([]domain.ContentHash, MaxBranchProjectionWeight+1)}})
	if _, found := cache.Get(key); found {
		test.Fatal("oversized projection cached")
	}
}

func TestBranchProjectionCacheFencesSourceGenerationAndEpoch(test *testing.T) {
	var cache BranchCache
	repo := legacyCacheHash(test.Name())
	key := BranchProjectionKey{Repo: repo, Graph: 1, Evidence: 1, Generation: legacyCacheGeneration(repo)}
	value := map[string]domain.BranchContext{"main": {SnapshotID: legacyCacheHash("old")}}
	cache.Put(key, value)
	for _, change := range []func(*BranchProjectionKey){
		func(next *BranchProjectionKey) { next.Generation.Sequence++ },
		func(next *BranchProjectionKey) { next.Generation.Epoch = "22345678-abcd-4321-9876-123456789abc" },
		func(next *BranchProjectionKey) { next.Generation = domain.RepositoryViewSourceCheckpoint{} },
		func(next *BranchProjectionKey) { next.Generation.RepoID = legacyCacheHash("foreign") },
		func(next *BranchProjectionKey) { next.Generation.Epoch = "00000000-0000-0000-0000-000000000000" },
		func(next *BranchProjectionKey) { next.Generation.Version++ },
		func(next *BranchProjectionKey) { next.Generation.Scope = domain.RepositoryViewSourceScope },
		func(next *BranchProjectionKey) { next.Generation.Sequence = -1 },
	} {
		next := key
		change(&next)
		if _, hit := cache.Get(next); hit {
			test.Fatalf("stale branch generation accepted: %+v", next)
		}
		if !next.cacheable() {
			cache.Put(next, value)
			if !reflect.DeepEqual(cache.Keys(), []BranchProjectionKey{key}) {
				test.Fatal("insufficient generation published")
			}
		}
	}
	next := key
	next.Generation.Sequence++
	cache.Put(next, value)
	cache.Put(key, value)
	if _, hit := cache.Get(next); hit || !reflect.DeepEqual(cache.Keys(), []BranchProjectionKey{key}) {
		test.Fatal("out-of-order completion crossed the exact-key generation fence")
	}
}

func TestBranchProjectionCacheWeightAndOrderedKeyCopies(test *testing.T) {
	var cache BranchCache
	if stats := cache.Stats(); stats.Entries != 0 || stats.Weight != 0 || len(cache.Keys()) != 0 {
		test.Fatalf("nonempty zero-value cache: %+v", stats)
	}
	value := map[string]domain.BranchContext{"main": {SnapshotIDs: make([]domain.ContentHash, 2), Roots: make([]domain.ContentHash, 3), Merges: make([]domain.BranchContextMerge, 4)}}
	var expected []BranchProjectionKey
	for index := range 9 {
		repo := legacyCacheHash(fmt.Sprint(index))
		key := BranchProjectionKey{Repo: repo, Generation: legacyCacheGeneration(repo)}
		expected = append(expected, key)
		cache.Put(key, value)
	}
	expected = expected[1:]
	keys := cache.Keys()
	if !reflect.DeepEqual(keys, expected) {
		test.Fatalf("FIFO retention order changed: %+v", keys)
	}
	keys[0].Origin = "caller mutation"
	if !reflect.DeepEqual(cache.Keys(), expected) || cache.Stats().Weight != 8*(1+2+3+16*4) {
		test.Fatal("key snapshots alias storage or branch weight changed")
	}
	key := expected[0]
	cache.Put(key, map[string]domain.BranchContext{"main": {SnapshotIDs: make([]domain.ContentHash, MaxBranchProjectionWeight-1)}})
	if stats := cache.Stats(); stats.Entries != 1 || stats.Weight != MaxBranchProjectionWeight {
		test.Fatalf("exact weight limit rejected or prior entries retained: %+v", stats)
	}
	cache.Put(key, map[string]domain.BranchContext{"main": {Roots: make([]domain.ContentHash, MaxBranchProjectionWeight)}})
	if stats := cache.Stats(); stats.Entries != 1 || stats.Weight != MaxBranchProjectionWeight {
		test.Fatalf("oversized replacement changed cache: %+v", stats)
	}
	other := expected[1]
	cache.Put(other, map[string]domain.BranchContext{"empty": {}})
	if !reflect.DeepEqual(cache.Keys(), []BranchProjectionKey{other}) || cache.Stats().Weight != 1 {
		test.Fatal("weight eviction failed")
	}
	if keys[1] != expected[1] {
		test.Fatal("cache writes mutated an earlier key snapshot")
	}
}

func TestCloneBranchContextsPreservesEmptyShape(test *testing.T) {
	if cloned := CloneBranchContexts(nil); cloned == nil || len(cloned) != 0 {
		test.Fatal("nil input must clone to an owned empty map")
	}
	cloned := CloneBranchContexts(map[string]domain.BranchContext{"main": {BranchID: "branch", CodeCommit: "commit", Reason: "reason", SnapshotID: legacyCacheHash("snapshot")}})
	branch := cloned["main"]
	if branch.BranchID != "branch" || branch.CodeCommit != "commit" || branch.Reason != "reason" || branch.SnapshotID != legacyCacheHash("snapshot") || branch.SnapshotIDs == nil || branch.Roots == nil || branch.Merges == nil {
		test.Fatal("clone changed scalar fields or nil-to-empty slice behavior")
	}
}
