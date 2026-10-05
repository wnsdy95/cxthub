package domain

import (
	"bytes"
	"context"
	"encoding/json"
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
	mu     sync.Mutex
	events map[canonicalEventKey]int
	order  []canonicalEventKey
	next   int
}

func (v *CanonicalDocVerifier) event(ctx context.Context, version string, raw []byte, pending *[]canonicalEventProof) (int, error) {
	key := canonicalEventKey{HashContent(raw), version}
	v.mu.Lock()
	seq, found := v.events[key]
	v.mu.Unlock()
	if found {
		return seq, ctx.Err()
	}
	var event CIREvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return 0, ErrIntegrity
	}
	doc := CIRDocument{Envelope: CIREnvelope{CIRVersion: version}, Events: []CIREvent{event}}
	if err := ValidateCIRVersion(doc); err != nil {
		return 0, err
	}
	// Replacement history has its own stable sequence ordering. Reuse the same
	// canonical rules as ordinary typed document validation, including union and
	// explicit optional-field presence checks.
	event = canonicalEvents(doc.Events)[0]
	canonical, err := canonicalJSON(event)
	if err != nil || !bytes.Equal(canonical, raw) {
		return 0, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Do not evict proofs that a later event in this same document may need.
	// Admission is bounded independently of document size and follows complete
	// document validation. Rejection or cancellation observed during validation
	// therefore discards the pending admissions without churning the cache.
	if len(*pending) < maxCanonicalEventProofs {
		*pending = append(*pending, canonicalEventProof{key, event.Seq})
	}
	return event.Seq, nil
}

func (v *CanonicalDocVerifier) remember(pending []canonicalEventProof) {
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
	if ValidateContentHash(hash) != nil || HashContent(raw) != hash {
		return zero, ErrIntegrity
	}
	env, stream, ok := canonicalStream(raw)
	if !ok {
		return zero, ErrIntegrity
	}
	var envelope CIREnvelope
	if json.Unmarshal(env, &envelope) != nil {
		return zero, ErrIntegrity
	}
	if err := ValidateCIRVersion(CIRDocument{Envelope: envelope}); err != nil {
		return zero, err
	}
	canonical, err := canonicalJSON(envelope)
	if err != nil || !bytes.Equal(canonical, env) {
		return zero, ErrIntegrity
	}
	// Framing has already validated JSON. Borrow each event slice without a
	// second JSON decoder or a transcript-sized RawMessage array. Only a cold
	// event needs typed decoding and canonicalization.
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
		return zero, err
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
	start, depth := 0, 0
	quoted, escaped := false, false
	for i, ch := range stream {
		if i%(64<<10) == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if quoted {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				quoted = false
			}
			continue
		}
		switch ch {
		case '"':
			quoted = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 0 {
				if err := visit(stream[start:i]); err != nil {
					return err
				}
				start = i + 1
			}
		}
	}
	if start < len(stream) {
		return visit(stream[start:])
	}
	return ctx.Err()
}
