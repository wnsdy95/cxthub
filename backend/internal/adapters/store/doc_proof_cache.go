package store

import (
	"context"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const maxDocProofs = 2048

// Hash-only, bounded, disposable proofs. No transcript bytes or durable trusted
// flag survive a call; ownership/existence are read through storage every time.
type docProofKey struct{ repo, expected, representation domain.ContentHash }
type docProofCache struct {
	mu     sync.Mutex
	proofs map[docProofKey]domain.VerifiedDocReference
	order  []docProofKey
	next   int
}

func (c *docProofCache) get(key docProofKey) (domain.VerifiedDocReference, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.proofs[key]
	return p, ok
}
func (c *docProofCache) put(key docProofKey, p domain.VerifiedDocReference) {
	if !p.Valid() || p.Hash() != key.expected {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.proofs == nil {
		c.proofs = map[docProofKey]domain.VerifiedDocReference{}
	}
	if _, ok := c.proofs[key]; ok {
		return
	}
	if len(c.order) < maxDocProofs {
		c.order = append(c.order, key)
	} else {
		delete(c.proofs, c.order[c.next])
		c.order[c.next] = key
		c.next = (c.next + 1) % maxDocProofs
	}
	c.proofs[key] = p
}
func (c *docProofCache) verify(ctx context.Context, repo, hash domain.ContentHash, raw []byte) (domain.VerifiedDocReference, error) {
	if err := ctx.Err(); err != nil {
		return domain.VerifiedDocReference{}, err
	}
	key := docProofKey{repo: repo, expected: hash, representation: domain.HashContent(raw)}
	if p, ok := c.get(key); ok {
		if err := ctx.Err(); err != nil {
			return domain.VerifiedDocReference{}, err
		}
		return p, nil
	}
	p, err := domain.VerifyStoredDocBytes(hash, raw)
	if err != nil {
		return domain.VerifiedDocReference{}, err
	}
	c.put(key, p)
	// A valid immutable proof may remain a disposable hint after cancellation;
	// cancellation still rejects this operation and never acknowledges a write.
	if err := ctx.Err(); err != nil {
		return domain.VerifiedDocReference{}, err
	}
	return p, nil
}
