package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"sync"
)

type branchProjectionKey struct {
	repo            domain.ContentHash
	graph, evidence uint64
	origin          string
}
type branchProjectionEntry struct {
	key    branchProjectionKey
	value  map[string]domain.BranchContext
	weight int
}

// A bounded process-local optimization; revision identities come from the same
// committed MVCC read as the inputs. It never caches permissions or documents.
type branchProjectionCache struct {
	mu      sync.Mutex
	entries []branchProjectionEntry
	weight  int
}

const maxBranchProjectionWeight = 250000

func cloneBranchContexts(in map[string]domain.BranchContext) map[string]domain.BranchContext {
	out := make(map[string]domain.BranchContext, len(in))
	for k, v := range in {
		v.SnapshotIDs = append([]domain.ContentHash{}, v.SnapshotIDs...)
		v.Roots = append([]domain.ContentHash{}, v.Roots...)
		v.Merges = append([]domain.BranchContextMerge{}, v.Merges...)
		out[k] = v
	}
	return out
}
func (c *branchProjectionCache) get(key branchProjectionKey) (map[string]domain.BranchContext, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if e.key == key {
			return cloneBranchContexts(e.value), true
		}
	}
	return nil, false
}
func (c *branchProjectionCache) put(key branchProjectionKey, value map[string]domain.BranchContext) {
	weight := len(value)
	for _, v := range value {
		weight += len(v.SnapshotIDs) + len(v.Roots) + 16*len(v.Merges)
	}
	if weight > maxBranchProjectionWeight {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Preserve only one committed generation per repository. Out-of-order reader
	// completion can evict a newer entry, but can never return it for an older key.
	for i := 0; i < len(c.entries); {
		if c.entries[i].key.repo == key.repo {
			c.weight -= c.entries[i].weight
			c.entries = append(c.entries[:i], c.entries[i+1:]...)
		} else {
			i++
		}
	}
	for len(c.entries) > 0 && (len(c.entries) >= 8 || c.weight+weight > maxBranchProjectionWeight) {
		c.weight -= c.entries[0].weight
		c.entries = c.entries[1:]
	}
	c.entries = append(c.entries, branchProjectionEntry{key, cloneBranchContexts(value), weight})
	c.weight += weight
}
