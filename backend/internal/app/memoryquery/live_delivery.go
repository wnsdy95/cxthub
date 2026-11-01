package memoryquery

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A delivery proof must cover the whole selection. Its additional read budget
// is independent of page assessment so exhaustion cannot change existing page
// contents, cursor behavior or evidence quality. Omit the optional proof rather
// than hash a partial or budget-degraded projection. No provider reads occur.
const effectiveMemoryDeliveryBytes = 64 << 20

type DeliveryCache struct {
	mu      sync.Mutex
	entries []liveDeliveryCacheEntry
}
type liveDeliveryCacheEntry struct{ state, proof domain.ContentHash }

func (cache *DeliveryCache) Lookup(state domain.ContentHash) (domain.ContentHash, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, entry := range cache.entries {
		if entry.state == state {
			return entry.proof, true
		}
	}
	return "", false
}
func (cache *DeliveryCache) Store(state, proof domain.ContentHash) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, entry := range cache.entries {
		if entry.state == state {
			return
		}
	}
	if len(cache.entries) == 64 {
		cache.entries = cache.entries[1:]
	}
	cache.entries = append(cache.entries, liveDeliveryCacheEntry{state, proof})
}

func (cache *DeliveryCache) Len() int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return len(cache.entries)
}

func (queries *LiveQueries) effectiveMemoryProof(ctx context.Context, repo domain.ContentHash, source domain.RepositoryViewSourceCheckpoint, out domain.EffectiveMemoryPage, history []domain.HistoryEvent, items []domain.EffectiveMemoryItem, cache *liveContributionCache) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cacheable := validLiveGraphGeneration(repo, source) && queries.mode != LiveFrozen && queries.delivery != nil && queries.proofScope != nil && queries.proofScope.CacheableMemoryProof(ctx, repo)
	if cacheable {
		if proof, ok := queries.delivery.Lookup(out.StateHash); ok {
			return proof, nil
		}
	}
	evidence, err := queries.newLiveEvidence(ctx, repo, cache)
	if err != nil {
		return "", err
	}
	resolver, err := newMemoryIntegration(queries, evidence, repo, history)
	if err != nil {
		return "", err
	}
	proof, err := effectiveMemoryDeliveryHash(ctx, resolver, out, items)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if cacheable {
		queries.delivery.Store(out.StateHash, proof)
	}
	return proof, nil
}

func effectiveMemoryDeliveryHash(ctx context.Context, resolver *memoryIntegration, out domain.EffectiveMemoryPage, items []domain.EffectiveMemoryItem) (domain.ContentHash, error) {
	complete := make([]domain.EffectiveMemoryItem, 0, len(items))
	used := 0
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if item.Kind == "code" {
			var err error
			item, err = resolver.assess(ctx, item, out.Selection.CodeCommit, out.Revision)
			if err != nil {
				return "", err
			}
			if resolver.evidence.Limited() {
				return "", nil
			}
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return "", err
		}
		if len(raw) > MaxPageBytes || len(raw) > effectiveMemoryDeliveryBytes-used {
			return "", nil
		}
		used += len(raw)
		complete = append(complete, item)
	}
	return domain.EffectiveMemoryDeliveryStateHash(ctx, resolver.repo, resolver.evidence.origin, out, complete)
}
