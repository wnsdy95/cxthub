package domain

import (
	"context"
	"sync"
)

const storedEventProofLimit = 65536

type storedEventKey struct {
	hash    [32]byte
	version string
}
type storedEventProof struct {
	key storedEventKey
	seq int
}

// StoredDocumentVerifier reuses bounded proofs of exact canonical event bytes.
// Every call still reads its supplied bytes, checks the document identity, CIR
// version, envelope, event order and complete JSON depth. No payload is retained.
// Its zero value is ready for concurrent use; do not copy it after first use.
type StoredDocumentVerifier struct {
	mu         sync.Mutex
	events     map[storedEventKey]int
	order      []storedEventKey
	next       int
	firstBytes int // last successful first-event length; predicts reuse only
}

func (v *StoredDocumentVerifier) lookup(key storedEventKey) (int, bool) {
	if v == nil {
		return 0, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	seq, ok := v.events[key]
	return seq, ok
}

func (v *StoredDocumentVerifier) remember(ctx context.Context, proofs []storedEventProof, firstBytes *int) error {
	if v == nil {
		return ctx.Err()
	}
	v.mu.Lock()
	// Cancellation may arrive while waiting for a concurrent admission. Check
	// Done under the metadata lock, but never call an arbitrary context's Err
	// method while locked. Failed/canceled calls must not insert or evict proofs.
	select {
	case <-ctx.Done():
		v.mu.Unlock()
		return ctx.Err()
	default:
	}
	defer v.mu.Unlock()
	// Only legacy verification predicts a first-event boundary. Root admission
	// leaves the latest concurrent legacy hint intact, including an explicit 0.
	if firstBytes != nil {
		v.firstBytes = *firstBytes
	}
	if v.events == nil {
		v.events = make(map[storedEventKey]int)
	}
	for _, proof := range proofs {
		if _, ok := v.events[proof.key]; ok {
			continue
		}
		if len(v.order) < storedEventProofLimit {
			v.order = append(v.order, proof.key)
		} else {
			delete(v.events, v.order[v.next])
			v.order[v.next] = proof.key
			v.next = (v.next + 1) % storedEventProofLimit
		}
		v.events[proof.key] = proof.seq
	}
	return nil
}
