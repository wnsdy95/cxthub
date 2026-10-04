package app

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// A delivery proof must cover the whole selection. Its additional read budget
// is independent of page assessment so exhaustion cannot change existing page
// contents, cursor behavior or evidence quality. Omit the optional proof rather
// than hash a partial or budget-degraded projection. No provider reads occur.
const effectiveMemoryDeliveryBytes = 64 << 20

// Only hashes are retained, at most 64 generations/selections. The key is the
// existing full state hash (repo, origin, selection, content mode, lineage,
// accepted history and graph/evidence clocks), NOT the semantic delivery hash.
// Reads still authorize, pin a repository generation, resolve lineage, validate
// cursors and assess their own page. Legacy stores without clocks do not cache.
// A nested write may see uncommitted evidence before its revision advances;
// neither cache reads nor writes are allowed in that context.
type effectiveMemoryDeliveryCache struct {
	mu      sync.Mutex
	entries []effectiveMemoryDeliveryCacheEntry
}
type effectiveMemoryDeliveryCacheEntry struct{ state, proof domain.ContentHash }

func (c *effectiveMemoryDeliveryCache) get(state domain.ContentHash) (domain.ContentHash, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.state == state {
			return entry.proof, true
		}
	}
	return "", false
}
func (c *effectiveMemoryDeliveryCache) put(state, proof domain.ContentHash) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.state == state {
			return
		}
	}
	if len(c.entries) == 64 {
		c.entries = c.entries[1:]
	}
	c.entries = append(c.entries, effectiveMemoryDeliveryCacheEntry{state, proof})
}

func (s *Service) effectiveMemoryProof(ctx context.Context, repo domain.ContentHash, out domain.EffectiveMemoryPage, history []domain.HistoryEvent, items []domain.EffectiveMemoryItem) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	_, insideWrite := ctx.Value(afterCommitKey{}).(*afterCommitActions)
	cacheable := (out.Revision.Graph != 0 || out.Revision.Evidence != 0) && !insideWrite
	if _, transactional := s.meta.(outbound.RepositoryTransactions); transactional {
		// RepositoryTransactions alone cannot distinguish a read snapshot from
		// raw WithinRepository nesting (which has no afterCommitKey). Unknown
		// transaction modes must never reuse or publish a process-wide proof.
		mode, known := s.meta.(outbound.RepositoryTransactionState)
		cacheable = cacheable && known && mode.InReadOnlyTransaction(ctx)
	}
	if cacheable {
		if proof, ok := s.deliveryCache.get(out.StateHash); ok {
			return proof, nil
		}
	}
	evidence, err := s.newCodeEvidence(ctx, repo)
	if err != nil {
		return "", err
	}
	resolver, err := newMemoryIntegration(s, evidence, repo, history)
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
		s.deliveryCache.put(out.StateHash, proof)
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
			if resolver.evidence.limited {
				return "", nil
			}
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return "", err
		}
		if len(raw) > effectiveMemoryPageBytes || len(raw) > effectiveMemoryDeliveryBytes-used {
			return "", nil
		}
		used += len(raw)
		complete = append(complete, item)
	}
	return domain.EffectiveMemoryDeliveryStateHash(ctx, resolver.repo, resolver.evidence.origin, out, complete)
}
