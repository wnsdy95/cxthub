package repositoryquery

import (
	"crypto/sha256"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const GraphProjectionCacheBytes = 8 << 20
const GraphProjectionCacheEntries = 4
const MaxBranchProjectionWeight = 250000

type GraphProjectionKey struct {
	Repo     domain.ContentHash
	Revision domain.RepositoryRevision
	Source   domain.RepositoryViewSourceCheckpoint
	Inputs   [sha256.Size]byte
	Position string
	Origin   string
}

type graphProjectionEntry struct {
	key      GraphProjectionKey
	value    []byte
	retained int
}

type GraphCache struct {
	mu       sync.Mutex
	entries  []graphProjectionEntry
	bytes    int
	retained int
	building map[GraphProjectionKey]bool
}

func (cache *GraphCache) Get(key GraphProjectionKey) ([]byte, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, entry := range cache.entries {
		if entry.key == key {
			return entry.value, true
		}
	}
	return nil, false
}

func (cache *GraphCache) Lookup(key GraphProjectionKey) ([]byte, func(), error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, entry := range cache.entries {
		if entry.key == key {
			return entry.value, nil, nil
		}
	}
	if cache.building[key] || len(cache.building) >= 2 {
		return nil, nil, domain.ErrResourceOverloaded
	}
	if cache.building == nil {
		cache.building = make(map[GraphProjectionKey]bool)
	}
	cache.building[key] = true
	return nil, func() {
		cache.mu.Lock()
		delete(cache.building, key)
		cache.mu.Unlock()
	}, nil
}

func (cache *GraphCache) Put(key GraphProjectionKey, encoded []byte) {
	if len(encoded) > GraphProjectionCacheBytes {
		return
	}
	estimate := projectionCacheCopy{}
	estimate.graphKey(key)
	if len(encoded) <= 32<<10 {
		estimate.array(len(encoded), 1)
	} else {
		estimate.add(1, 8<<10)
		estimate.add(len(encoded), 1)
	}
	if estimate.full {
		return
	}
	owned := projectionCacheCopy{owned: true}
	key = owned.graphKey(key)
	value := make([]byte, len(encoded))
	copy(value, encoded)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for index := 0; index < len(cache.entries); {
		entry := cache.entries[index]
		if entry.key.Repo == key.Repo && (entry.key.Revision != key.Revision || entry.key.Position == key.Position) {
			cache.bytes -= len(entry.value)
			cache.retained -= entry.retained
			copy(cache.entries[index:], cache.entries[index+1:])
			cache.entries[len(cache.entries)-1] = graphProjectionEntry{}
			cache.entries = cache.entries[:len(cache.entries)-1]
		} else {
			index++
		}
	}
	for len(cache.entries) > 0 && (len(cache.entries) >= GraphProjectionCacheEntries || cache.bytes+len(value) > GraphProjectionCacheBytes || cache.retained+estimate.bytes > ProjectionRetentionBytes) {
		cache.bytes -= len(cache.entries[0].value)
		cache.retained -= cache.entries[0].retained
		copy(cache.entries, cache.entries[1:])
		cache.entries[len(cache.entries)-1] = graphProjectionEntry{}
		cache.entries = cache.entries[:len(cache.entries)-1]
	}
	cache.entries = append(cache.entries, graphProjectionEntry{key: key, value: value, retained: estimate.bytes})
	cache.bytes += len(value)
	cache.retained += estimate.bytes
}

func (cache *GraphCache) RetainedBytes() int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.retained
}

func (cache *GraphCache) Keys() []GraphProjectionKey {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	keys := make([]GraphProjectionKey, len(cache.entries))
	for index, entry := range cache.entries {
		keys[index] = entry.key
	}
	return keys
}

func (cache *GraphCache) Stats() struct{ Entries, Bytes, Building int } {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return struct{ Entries, Bytes, Building int }{len(cache.entries), cache.bytes, len(cache.building)}
}

type BranchProjectionKey struct {
	Repo            domain.ContentHash
	Graph, Evidence uint64
	Origin          string
	Generation      domain.RepositoryViewSourceCheckpoint
}

func (key BranchProjectionKey) cacheable() bool {
	return ValidProjectionGraphGeneration(key.Repo, key.Generation)
}

type branchProjectionEntry struct {
	key      BranchProjectionKey
	value    map[string]domain.BranchContext
	weight   int
	retained int
}

type BranchCache struct {
	mu       sync.Mutex
	entries  []branchProjectionEntry
	weight   int
	retained int
}

func CloneBranchContexts(input map[string]domain.BranchContext) map[string]domain.BranchContext {
	output := make(map[string]domain.BranchContext, len(input))
	for branch, value := range input {
		value.SnapshotIDs = append([]domain.ContentHash{}, value.SnapshotIDs...)
		value.Roots = append([]domain.ContentHash{}, value.Roots...)
		value.Merges = append([]domain.BranchContextMerge{}, value.Merges...)
		output[branch] = value
	}
	return output
}

func (cache *BranchCache) Get(key BranchProjectionKey) (map[string]domain.BranchContext, bool) {
	if !key.cacheable() {
		return nil, false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, entry := range cache.entries {
		if entry.key == key {
			return CloneBranchContexts(entry.value), true
		}
	}
	return nil, false
}

func (cache *BranchCache) Put(key BranchProjectionKey, value map[string]domain.BranchContext) {
	if !key.cacheable() {
		return
	}
	weight := len(value)
	for _, branch := range value {
		weight += len(branch.SnapshotIDs) + len(branch.Roots) + 16*len(branch.Merges)
	}
	if weight > MaxBranchProjectionWeight {
		return
	}
	estimate := projectionCacheCopy{}
	estimate.branchKey(key)
	estimate.branches(value)
	if estimate.full {
		return
	}
	owned := projectionCacheCopy{owned: true}
	key, value = owned.branchKey(key), owned.branches(value)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for index := 0; index < len(cache.entries); {
		if cache.entries[index].key.Repo == key.Repo {
			cache.weight -= cache.entries[index].weight
			cache.retained -= cache.entries[index].retained
			copy(cache.entries[index:], cache.entries[index+1:])
			cache.entries[len(cache.entries)-1] = branchProjectionEntry{}
			cache.entries = cache.entries[:len(cache.entries)-1]
		} else {
			index++
		}
	}
	for len(cache.entries) > 0 && (len(cache.entries) >= 8 || cache.weight+weight > MaxBranchProjectionWeight || cache.retained+estimate.bytes > ProjectionRetentionBytes) {
		cache.weight -= cache.entries[0].weight
		cache.retained -= cache.entries[0].retained
		copy(cache.entries, cache.entries[1:])
		cache.entries[len(cache.entries)-1] = branchProjectionEntry{}
		cache.entries = cache.entries[:len(cache.entries)-1]
	}
	cache.entries = append(cache.entries, branchProjectionEntry{key: key, value: value, weight: weight, retained: estimate.bytes})
	cache.weight += weight
	cache.retained += estimate.bytes
}

func (cache *BranchCache) RetainedBytes() int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.retained
}

func (cache *BranchCache) Keys() []BranchProjectionKey {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	keys := make([]BranchProjectionKey, len(cache.entries))
	for index, entry := range cache.entries {
		keys[index] = entry.key
	}
	return keys
}

func (cache *BranchCache) Stats() struct{ Entries, Weight int } {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return struct{ Entries, Weight int }{len(cache.entries), cache.weight}
}
