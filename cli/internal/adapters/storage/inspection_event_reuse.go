package storage

import (
	"crypto/sha256"
	"encoding/json"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

const (
	inspectionEventProofLimit = 65536
	inspectionChunkLabelLimit = 8192
	eventProofMissLimit       = 128
	eventProofMissByteLimit   = 1 << 20
)

// Chunk labels predict useful repetition, not validity. Labels are registered
// only after a complete successful current-byte document verification.
type inspectionEventReuse struct {
	chunks     map[domain.ContentHash]struct{}
	chunkOrder []domain.ContentHash
	chunkNext  int
	proofs     eventProofCache
}

func (r *inspectionEventReuse) forDocument(chunks []domain.ContentHash) *eventProofDecoder {
	if r == nil || len(chunks) == 0 {
		return nil
	}
	repeated := 0
	for _, hash := range chunks {
		if _, ok := r.chunks[hash]; ok {
			repeated++
		}
	}
	if repeated < (len(chunks)+1)/2 {
		return nil
	}
	return &eventProofDecoder{cache: &r.proofs, warming: len(r.proofs.values) == 0}
}

func (r *inspectionEventReuse) observe(chunks []domain.ContentHash) {
	if r == nil {
		return
	}
	if r.chunks == nil {
		r.chunks = make(map[domain.ContentHash]struct{})
	}
	for _, hash := range chunks {
		if _, ok := r.chunks[hash]; ok {
			continue
		}
		if len(r.chunkOrder) < inspectionChunkLabelLimit {
			r.chunkOrder = append(r.chunkOrder, hash)
		} else {
			delete(r.chunks, r.chunkOrder[r.chunkNext])
			r.chunkOrder[r.chunkNext] = hash
			r.chunkNext = (r.chunkNext + 1) % len(r.chunkOrder)
		}
		r.chunks[hash] = struct{}{}
	}
}

// Only SHA-256 markers of exact current JSON bytes are retained. Parsed maps
// are not proof keys: duplicate fields and invalid typed values can normalize
// to the same map as a valid event. The cache has no payload or process lifetime.
type eventProofCache struct {
	values map[[32]byte]struct{}
	order  [][32]byte
	next   int
}

func (c *eventProofCache) contains(hash [32]byte) bool {
	_, ok := c.values[hash]
	return ok
}

func (c *eventProofCache) remember(hash [32]byte) {
	if c.contains(hash) {
		return
	}
	if c.values == nil {
		c.values = make(map[[32]byte]struct{})
	}
	if len(c.order) < inspectionEventProofLimit {
		c.order = append(c.order, hash)
	} else {
		delete(c.values, c.order[c.next])
		c.order[c.next] = hash
		c.next = (c.next + 1) % len(c.order)
	}
	c.values[hash] = struct{}{}
}

type eventProofDecoder struct {
	cache           *eventProofCache
	warming         bool
	direct          bool
	consecutiveMiss int
	missBytes       int
}

func (p *eventProofDecoder) decode(decoder *json.Decoder) error {
	if p == nil || p.direct {
		var event domain.Event
		return decoder.Decode(&event)
	}
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	hash := sha256.Sum256(raw)
	if p.cache.contains(hash) {
		p.consecutiveMiss = 0
		return nil
	}
	var event domain.Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	p.cache.remember(hash)
	p.consecutiveMiss++
	if p.missBytes < eventProofMissByteLimit {
		if len(raw) >= eventProofMissByteLimit-p.missBytes {
			p.missBytes = eventProofMissByteLimit
		} else {
			p.missBytes += len(raw)
		}
	}
	// Warm one bounded cache first. Afterwards, repeated labels with unique
	// whole events or FIFO scan thrashing must not add raw-copy/hash work for
	// the rest of a document. This is a cost heuristic, never a validation skip.
	if !p.warming || len(p.cache.values) == inspectionEventProofLimit {
		p.direct = p.consecutiveMiss >= eventProofMissLimit || p.missBytes >= eventProofMissByteLimit
	}
	return nil
}
