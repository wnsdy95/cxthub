package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"sync"
)

type contextSegmentCacheKey struct {
	repo                                                 domain.ContentHash
	revision                                             domain.RepositoryRevision
	branch, position, code, scope, defaultBranch, origin string
}

func segmentCacheKey(repo domain.ContentHash, r domain.Repo, revision domain.RepositoryRevision, in domain.ContextSelection) contextSegmentCacheKey {
	return contextSegmentCacheKey{repo, revision, in.Branch, in.Position, in.CodeCommit, in.Scope, r.DefaultBranch, r.GitRemoteURL}
}

type contextSegmentBasis struct {
	view     domain.ContextQueryView
	selected map[domain.ContentHash]domain.Snapshot
	bindings map[domain.ContentHash][]domain.CommitContextBinding
}
type contextSegmentCacheEntry struct {
	key    contextSegmentCacheKey
	value  contextSegmentBasis
	weight int
}

// Immutable application projections, never authentication decisions or raw
// documents. Every hit follows the normal authorization and pinned revision read.
type contextSegmentCache struct {
	mu      sync.Mutex
	entries []contextSegmentCacheEntry
	weight  int
}

func (c *contextSegmentCache) get(key contextSegmentCacheKey) (contextSegmentBasis, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.key == key {
			return entry.value, true
		}
	}
	return contextSegmentBasis{}, false
}
func (c *contextSegmentCache) put(key contextSegmentCacheKey, value contextSegmentBasis) {
	weight := len(value.view.Snapshots) + len(value.view.History)
	if weight > 250000 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.key == key {
			return
		}
	}
	for len(c.entries) > 0 && (len(c.entries) >= 8 || c.weight+weight > 250000) {
		c.weight -= c.entries[0].weight
		c.entries = c.entries[1:]
	}
	c.entries = append(c.entries, contextSegmentCacheEntry{key, value, weight})
	c.weight += weight
}
