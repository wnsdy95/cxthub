package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// verifyStored binds a disposable proof to the exact current representation.
// readChunk must read repository-owned stored bytes, without decompression or
// an existence-only shortcut. Every call still reads all distinct chunks; a
// warm proof saves decompression, cumulative assembly and CIR validation only.
func (c *docProofCache) verifyStored(ctx context.Context, repo, hash domain.ContentHash, stored []byte, readChunk func(context.Context, domain.ContentHash) ([]byte, error)) (domain.VerifiedDocReference, error) {
	if err := ctx.Err(); err != nil {
		return domain.VerifiedDocReference{}, err
	}
	data, err := docDecompress(stored)
	if err != nil {
		return domain.VerifiedDocReference{}, domain.ErrIntegrity
	}
	man, chunked := domain.ParseDocChunkManifest(data)
	if !chunked {
		return c.verify(ctx, repo, hash, data)
	}
	// Fixed-width hashes after a domain separator bind the exact descriptor and
	// each distinct chunk identity/body pair. The descriptor includes ordering,
	// duplicate occurrences, format and envelope. Repacking is a cache miss.
	fingerprint := sha256.New()
	fingerprint.Write([]byte("cxt-stored-doc-proof-v1\x00"))
	fingerprint.Write([]byte(domain.HashContent(stored)))
	bodies := make(map[domain.ContentHash][]byte, len(man.Chunks))
	for _, ch := range man.Chunks {
		if err := ctx.Err(); err != nil {
			return domain.VerifiedDocReference{}, err
		}
		if err := validateHash(ch); err != nil {
			return domain.VerifiedDocReference{}, domain.ErrIntegrity
		}
		if _, seen := bodies[ch]; seen {
			continue
		}
		raw, err := readChunk(ctx, ch)
		if err != nil {
			return domain.VerifiedDocReference{}, err
		}
		bodies[ch] = raw
		fingerprint.Write([]byte(ch))
		fingerprint.Write([]byte(domain.HashContent(raw)))
	}
	if err := ctx.Err(); err != nil {
		return domain.VerifiedDocReference{}, err
	}
	key := docProofKey{repo: repo, expected: hash, representation: domain.ContentHash("sha256:" + hex.EncodeToString(fingerprint.Sum(nil)))}
	if p, ok := c.get(key); ok {
		if err := ctx.Err(); err != nil {
			return domain.VerifiedDocReference{}, err
		}
		return p, nil
	}
	// Validate precisely the bytes fingerprinted above. Reading them again here
	// could attach a valid proof to different bytes during a concurrent change.
	chunks := make([][]byte, 0, len(man.Chunks))
	decoded := make(map[domain.ContentHash][]byte, len(bodies))
	for _, ch := range man.Chunks {
		if err := ctx.Err(); err != nil {
			return domain.VerifiedDocReference{}, err
		}
		body, seen := decoded[ch]
		if !seen {
			body, err = docDecompress(bodies[ch])
			if err != nil {
				return domain.VerifiedDocReference{}, domain.ErrIntegrity
			}
			decoded[ch] = body
		}
		chunks = append(chunks, body)
	}
	raw, err := domain.AssembleDocChunks(man, chunks, hash)
	if err != nil {
		return domain.VerifiedDocReference{}, domain.ErrIntegrity
	}
	// Assembly has checked the complete canonical hash. PutVerifiedDoc may
	// already have a typed proof for it; otherwise validate the CIR now. Keep
	// only one new physical proof, not a second canonical entry on every miss.
	p, ok := c.get(docProofKey{repo: repo, expected: hash, representation: hash})
	if !ok {
		p, err = domain.VerifyStoredDocBytes(hash, raw)
		if err != nil {
			return domain.VerifiedDocReference{}, err
		}
	}
	c.put(key, p)
	if err := ctx.Err(); err != nil {
		return domain.VerifiedDocReference{}, err
	}
	return p, nil
}
