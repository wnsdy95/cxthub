package domain

import (
	"bytes"
	"context"
	"sync"
)

const maxCanonicalEventProofs = 65536

type canonicalEventKey struct {
	hash    ContentHash
	version string
}

type canonicalEventProof struct {
	key canonicalEventKey
	seq int
}

// CanonicalDocVerifier reuses only proofs of exact event bytes under the same
// CIR version. It retains bounded hashes/sequence metadata, never transcripts.
// Its zero value is ready for concurrent use; it must not be copied after use.
// A process restart or eviction loses speed, not correctness or stored data.
type CanonicalDocVerifier struct {
	mu       sync.Mutex
	events   map[canonicalEventKey]int
	order    []canonicalEventKey
	next     int
	uncached bool
}

func NewCanonicalDocVerifierWithoutCache() *CanonicalDocVerifier {
	return &CanonicalDocVerifier{uncached: true}
}

func (v *CanonicalDocVerifier) event(ctx context.Context, version string, raw []byte, pending *[]canonicalEventProof) (int, error) {
	if ctx.Err() != nil {
		return v.eventWithKey(ctx, canonicalEventKey{HashContent(raw), version}, raw, pending)
	}
	hash, err := HashContentContext(ctx, raw)
	if err != nil {
		return 0, err
	}
	key := canonicalEventKey{hash, version}
	return v.eventWithKey(ctx, key, raw, pending)
}

func (v *CanonicalDocVerifier) eventWithKey(ctx context.Context, key canonicalEventKey, raw []byte, pending *[]canonicalEventProof) (int, error) {
	if v.uncached {
		return verifyCanonicalEvent(ctx, key.version, raw)
	}
	v.mu.Lock()
	seq, found := v.events[key]
	v.mu.Unlock()
	if found {
		return seq, ctx.Err()
	}
	seq, err := verifyCanonicalEvent(ctx, key.version, raw)
	if err != nil {
		return 0, err
	}
	if len(*pending) < maxCanonicalEventProofs {
		*pending = append(*pending, canonicalEventProof{key, seq})
	}
	return seq, nil
}

func verifyCanonicalEvent(ctx context.Context, version string, raw []byte) (int, error) {
	if ctx.Err() != nil {
		return verifyCanonicalEventBytes(ctx, version, raw)
	}
	projected, err := canonicalEventProjection(ctx, raw)
	if err != nil {
		return 0, err
	}
	if sequence, valid := canonicalMessageEventSequence(version, projected); valid {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return sequence, nil
	}
	return verifyCanonicalEventBytes(ctx, version, projected)
}

func verifyCanonicalEventBytes(ctx context.Context, version string, raw []byte) (int, error) {
	event, err := decodeCanonicalEvent(raw)
	if err != nil {
		return 0, ErrIntegrity
	}
	doc := CIRDocument{Envelope: CIREnvelope{CIRVersion: version}, Events: []CIREvent{event}}
	if err := ValidateCIRVersion(doc); err != nil {
		return 0, err
	}
	if event.Replacement != nil {
		event.Replacement = canonicalEvents(event.Replacement)
	}
	canonical, err := canonicalDecodedEventJSON(event)
	if err != nil || !bytes.Equal(canonical, raw) {
		return 0, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return event.Seq, nil
}

func (v *CanonicalDocVerifier) remember(pending []canonicalEventProof) {
	if v.uncached {
		return
	}
	if len(pending) == 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.events == nil {
		v.events = make(map[canonicalEventKey]int)
	}
	for _, proof := range pending {
		key := proof.key
		if _, exists := v.events[key]; exists {
			continue
		}
		if len(v.order) < maxCanonicalEventProofs {
			v.order = append(v.order, key)
		} else {
			delete(v.events, v.order[v.next])
			v.order[v.next] = key
			v.next = (v.next + 1) % maxCanonicalEventProofs
		}
		v.events[key] = proof.seq
	}
}

// Verify accepts canonical wire bytes only. Legacy noncanonical storage reads
// continue through VerifyStoredDocBytes. Whole-body hash, envelope, CIR version,
// exact event bytes and stable sequence order are checked on every call. A
// cached event is neither a repository ownership proof nor a publication receipt.
func (v *CanonicalDocVerifier) Verify(ctx context.Context, hash ContentHash, raw []byte) (VerifiedSessionDoc, error) {
	var zero VerifiedSessionDoc
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if ValidateContentHash(hash) != nil {
		return zero, ErrIntegrity
	}
	actual, err := HashContentContext(ctx, raw)
	if err != nil {
		return zero, err
	}
	if actual != hash {
		return zero, ErrIntegrity
	}
	env, stream, err := canonicalVerificationFrame(ctx, raw)
	if err != nil {
		return zero, err
	}
	envelope, err := decodeCanonicalCIREnvelope(env)
	if err != nil {
		return zero, canonicalVerificationFailure(ctx, raw, err)
	}
	previous, havePrevious := 0, false
	var pending []canonicalEventProof
	err = visitCanonicalEvents(ctx, stream, func(body []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		seq, err := v.event(ctx, envelope.CIRVersion, body, &pending)
		if err != nil {
			return err
		}
		if havePrevious && seq < previous {
			return ErrIntegrity
		}
		previous, havePrevious = seq, true
		return nil
	})
	if err != nil {
		return zero, canonicalVerificationFailure(ctx, raw, err)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	v.remember(pending)
	// An owned immutable copy preserves the same value contract as VerifySessionDoc.
	return VerifiedSessionDoc{hash: hash, canonical: string(raw)}, nil
}

// Input is the compact, syntactically validated interior of the events array.
// Commas in nested replacement histories, tool JSON and quoted text are data.
func visitCanonicalEvents(ctx context.Context, stream []byte, visit func([]byte) error) error {
	return visitCanonicalParts(ctx, [][]byte{stream}, func(span canonicalSpan) error {
		return visit(stream[span.offset : span.offset+span.length])
	})
}
