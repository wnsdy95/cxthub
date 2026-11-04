package repositoryquery

import (
	"context"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type contextSegmentCacheKey struct {
	repo                                                 domain.ContentHash
	revision                                             domain.RepositoryRevision
	source                                               domain.RepositoryViewSourceCheckpoint
	branch, position, code, scope, defaultBranch, origin string
}

func (key contextSegmentCacheKey) cacheable() bool {
	return ValidProjectionGraphGeneration(key.repo, key.source)
}

func (queries *ContextQueries) segmentCacheKey(ctx context.Context, repo domain.ContentHash, metadata domain.Repo, revision domain.RepositoryRevision, in domain.ContextSelection) (contextSegmentCacheKey, error) {
	key := contextSegmentCacheKey{repo: repo, revision: revision, branch: in.Branch, position: in.Position, code: in.CodeCommit, scope: in.Scope, defaultBranch: metadata.DefaultBranch, origin: metadata.GitRemoteURL}
	if in.SegmentLimit == 0 {
		return key, nil
	}
	generation, complete, err := ReadProjectionGraphSourceGeneration(ctx, repo, queries.cacheScope, queries.generations, queries.captured)
	if err != nil || !complete {
		return key, err
	}
	key.source = generation
	return key, nil
}

type contextSegmentBasis struct {
	view     domain.ContextQueryView
	selected map[domain.ContentHash]domain.Snapshot
	bindings map[domain.ContentHash][]domain.CommitContextBinding
}

type contextSegmentCacheEntry struct {
	key      contextSegmentCacheKey
	value    contextSegmentBasis
	weight   int
	retained int
}

type SegmentCache struct {
	mu       sync.Mutex
	entries  []contextSegmentCacheEntry
	weight   int
	retained int
}

type SegmentCacheStats struct {
	Entries int
	Weight  int
}

func (cache *SegmentCache) Stats() SegmentCacheStats {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return SegmentCacheStats{Entries: len(cache.entries), Weight: cache.weight}
}

func (cache *SegmentCache) get(key contextSegmentCacheKey) (contextSegmentBasis, bool) {
	if !key.cacheable() {
		return contextSegmentBasis{}, false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, entry := range cache.entries {
		if entry.key == key {
			return entry.value, true
		}
	}
	return contextSegmentBasis{}, false
}

func (cache *SegmentCache) put(key contextSegmentCacheKey, value contextSegmentBasis) {
	if !key.cacheable() {
		return
	}
	weight := len(value.view.Snapshots) + len(value.view.History)
	if weight > 250000 {
		return
	}
	estimate := projectionCacheCopy{}
	estimate.segmentKey(key)
	estimate.basis(value)
	if estimate.full {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, entry := range cache.entries {
		if entry.key == key {
			return
		}
	}
	owned := projectionCacheCopy{owned: true}
	key, value = owned.segmentKey(key), owned.basis(value)
	for len(cache.entries) > 0 && (len(cache.entries) >= 8 || cache.weight+weight > 250000 || cache.retained+estimate.bytes > ProjectionRetentionBytes) {
		cache.weight -= cache.entries[0].weight
		cache.retained -= cache.entries[0].retained
		copy(cache.entries, cache.entries[1:])
		cache.entries[len(cache.entries)-1] = contextSegmentCacheEntry{}
		cache.entries = cache.entries[:len(cache.entries)-1]
	}
	cache.entries = append(cache.entries, contextSegmentCacheEntry{key: key, value: value, weight: weight, retained: estimate.bytes})
	cache.weight += weight
	cache.retained += estimate.bytes
}

func (cache *SegmentCache) RetainedBytes() int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.retained
}
