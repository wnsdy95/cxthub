package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// Verify preserves legacy typed normalization for noncanonical stored documents.
// The caller must supply the current repository-owned bytes, not an ID-only hint.
func (v *StoredDocumentVerifier) Verify(ctx context.Context, want ContentHash, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.canonical(ctx, want, raw) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var cir CIRDocument
	if err := json.Unmarshal(raw, &cir); err != nil {
		return ErrInvalidCIR
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var pending []storedEventProof
	firstBytes := 0
	observe := func(body []byte, seq int) {
		if firstBytes == 0 {
			firstBytes = len(body)
		}
		if len(pending) < storedEventProofLimit {
			key := storedEventKey{sha256.Sum256(body), cir.Envelope.CIRVersion}
			pending = append(pending, storedEventProof{key, seq})
		}
	}
	if err := validateSessionDocHash(SessionDoc{Hash: want, CIR: cir}, observe); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	v.remember(pending, firstBytes)
	return nil
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

func (v *StoredDocumentVerifier) remember(proofs []storedEventProof, firstBytes int) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.firstBytes = firstBytes
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
}

// A miss is a request for the original normalizer, not weaker acceptance. Only
// complete successful canonical documents admit proofs; failed/canceled scans
// cannot evict proofs required later in the same scan or publish partial trust.
func (v *StoredDocumentVerifier) canonical(ctx context.Context, want ContentHash, raw []byte) bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	firstBytes := v.firstBytes
	v.mu.Unlock()
	if firstBytes <= 0 {
		return false
	}
	const prefix = `{"envelope":`
	const middle = `,"events":[`
	if !bytes.HasPrefix(raw, []byte(prefix)) || !bytes.HasSuffix(raw, []byte(`]}`)) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw[len(prefix):]))
	var envRaw json.RawMessage
	if decoder.Decode(&envRaw) != nil {
		return false
	}
	offset := len(prefix) + int(decoder.InputOffset())
	if !bytes.HasPrefix(raw[offset:], []byte(middle)) {
		return false
	}
	var envelope Envelope
	if json.Unmarshal(envRaw, &envelope) != nil || ValidateCIRVersion(CIRDocument{Envelope: envelope}) != nil {
		return false
	}
	normalized, err := canonicalJSON(envelope)
	if err != nil || !bytes.Equal(normalized, envRaw) {
		return false
	}
	stream := raw[offset+len(middle) : len(raw)-2]
	// Probe the previous successful boundary before scanning a potentially huge
	// unrelated first event. A stale length can only choose the slow path: even
	// a hit must pass the complete current framing, sequence and document hash.
	if firstBytes > len(stream) {
		return false
	}
	if _, found := v.lookup(storedEventKey{sha256.Sum256(stream[:firstBytes]), envelope.CIRVersion}); !found {
		return false
	}
	digest := sha256.New()
	digest.Write([]byte(prefix))
	digest.Write(normalized)
	digest.Write([]byte(middle))
	previous, havePrevious := 0, false
	var pending []storedEventProof
	ok := visitStoredEvents(ctx, stream, func(body []byte) bool {
		if ctx.Err() != nil {
			return false
		}
		key := storedEventKey{sha256.Sum256(body), envelope.CIRVersion}
		seq, found := v.lookup(key)
		if !found {
			// A first-event miss predicts an unrelated/cold document. Use the
			// original normalizer once and collect its already computed event
			// bytes, instead of paying for two representations of every event.
			// This heuristic chooses work only; it never authorizes a document.
			if !havePrevious {
				return false
			}
			var event Event
			if json.Unmarshal(body, &event) != nil || ValidateCIRVersion(CIRDocument{Envelope: envelope, Events: []Event{event}}) != nil {
				return false
			}
			canonical, err := canonicalJSON(canonicalEvents([]Event{event})[0])
			if err != nil || !bytes.Equal(canonical, body) {
				return false
			}
			seq = event.Seq
			if len(pending) < storedEventProofLimit {
				pending = append(pending, storedEventProof{key, seq})
			}
		}
		if havePrevious {
			if seq < previous {
				return false
			}
			digest.Write([]byte{','})
		}
		digest.Write(body)
		previous, havePrevious = seq, true
		return true
	})
	if !ok {
		return false
	}
	digest.Write([]byte(`]}`))
	if ContentHash("sha256:"+hex.EncodeToString(digest.Sum(nil))) != want || ctx.Err() != nil {
		return false
	}
	v.remember(pending, firstBytes)
	return true
}

// Framing mirrors the backend canonical event scanner. Every event must then
// pass typed canonical equality or an exact-byte proof. The stack accounts for
// the enclosing document and events array in encoding/json's 10,000-depth limit.
func visitStoredEvents(ctx context.Context, stream []byte, visit func([]byte) bool) bool {
	var stack [9998]byte
	depth, start := 0, 0
	quoted, escaped, comma, afterComma := false, false, false, false
	for i, ch := range stream {
		if i%(64<<10) == 0 && ctx.Err() != nil {
			return false
		}
		if quoted {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				quoted = false
			}
		} else if depth == 0 {
			if comma {
				if ch != ',' {
					return false
				}
				comma, afterComma = false, true
			} else {
				if ch != '{' {
					return false
				}
				start, depth, stack[0], afterComma = i, 1, '{', false
			}
		} else {
			switch ch {
			case '"':
				quoted = true
			case '{', '[':
				if depth == len(stack) {
					return false
				}
				stack[depth] = ch
				depth++
			case '}', ']':
				if (ch == '}' && stack[depth-1] != '{') || (ch == ']' && stack[depth-1] != '[') {
					return false
				}
				depth--
				if depth == 0 {
					if !visit(stream[start : i+1]) {
						return false
					}
					comma = true
				}
			case ' ', '\n', '\r', '\t':
				return false
			}
		}
	}
	return depth == 0 && !quoted && !afterComma && ctx.Err() == nil
}
