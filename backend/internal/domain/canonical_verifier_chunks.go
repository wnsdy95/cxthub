package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// VerifyChunks reads and hashes EVERY current manifest occurrence, even when
// its events or chunk hash were seen before. The v2 full canonical DocHash is
// streamed in O(bytes); only event semantic proofs are reused, not the full
// document hash. No cumulative canonical byte slice is built on the v2 path.
//
// Incoming v2 partitions may be arbitrary. Retained/exported chunks always use
// standard ChunkTarget boundaries so fixed-offset readers remain correct.
// Bounds are MaxDocJobChunks, MaxDocJobManifestBytes and MaxFinalizedDocBytes
// including the exact canonical framing. A loaded body may fill the remaining
// document budget; portable batch limits do not constrain legacy stored chunks.
// Loader buffers may be reused after the next call; all retained bytes are owned.
// Loader errors and cancellation are returned unchanged; invalid data is never
// admitted to the event cache. Loaders must themselves honor ctx during I/O.
//
// V1 (including the empty legacy format) intentionally uses AssembleDocChunks
// and Verify after bounded chunk loading, retaining the full-byte compatibility
// path. Its ChunkPlan still exports standard v2 chunks on demand.
func (v *CanonicalDocVerifier) VerifyChunks(ctx context.Context, want ContentHash, manifest DocChunkManifest, load func(context.Context, ContentHash) ([]byte, error)) (VerifiedSessionDoc, error) {
	return v.verifyChunks(ctx, want, manifest, load, MaxFinalizedDocBytes)
}

func (v *CanonicalDocVerifier) verifyChunks(ctx context.Context, want ContentHash, manifest DocChunkManifest, load func(context.Context, ContentHash) ([]byte, error), maxBytes int) (VerifiedSessionDoc, error) {
	var zero VerifiedSessionDoc
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if (load == nil && len(manifest.Chunks) > 0) || ValidateContentHash(want) != nil || !SupportedChunkFormat(manifest.Format) ||
		len(manifest.Chunks) > MaxDocJobChunks ||
		len(manifest.Envelope) == 0 || len(manifest.Envelope) > MaxDocJobManifestBytes {
		return zero, ErrIntegrity
	}
	// Snapshot before invoking untrusted/reusing loaders. The proof must describe
	// the same owned envelope and hash order that we actually checked.
	manifest.Envelope = bytes.Clone(manifest.Envelope)
	manifest.Chunks = append([]ContentHash(nil), manifest.Chunks...)
	for _, h := range manifest.Chunks {
		if ValidateContentHash(h) != nil {
			return zero, ErrIntegrity
		}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil || len(encoded) > MaxDocJobManifestBytes {
		return zero, ErrIntegrity
	}
	var envelope CIREnvelope
	if json.Unmarshal(manifest.Envelope, &envelope) != nil {
		return zero, ErrIntegrity
	}
	if err := ValidateCIRVersion(CIRDocument{Envelope: envelope}); err != nil {
		return zero, err
	}
	canonical, err := canonicalJSON(envelope)
	if err != nil || !bytes.Equal(canonical, manifest.Envelope) {
		return zero, ErrIntegrity
	}
	v2 := normalizeChunkFormat(manifest.Format) == ChunkFormatV2
	owned := &verifiedDocChunks{envelope: string(manifest.Envelope)}
	var legacy [][]byte
	var tail []byte
	retain := func(body []byte) {
		owned.stream.add(string(body))
		owned.hashes = append(owned.hashes, HashContent(body))
	}
	fullHash, chunkHash := sha256.New(), sha256.New()
	fullHash.Write([]byte(canonicalDocPrefix))
	fullHash.Write(manifest.Envelope)
	fullHash.Write([]byte(canonicalDocMiddle))
	total := canonicalFrameBytes + len(manifest.Envelope)
	if !v2 {
		// V1 inserts a comma between chunks, in addition to replacing each
		// internal newline with a comma. Count those bytes before allocating.
		total += max(0, len(manifest.Chunks)-1)
	}
	if total > maxBytes {
		return zero, ErrIntegrity
	}
	for _, h := range manifest.Chunks {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		body, err := load(ctx, h)
		if err != nil {
			return zero, err
		}
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		// Subtract before adding to avoid overflow for hostile body sizes.
		if len(body) > maxBytes-total {
			return zero, ErrIntegrity
		}
		total += len(body)
		chunkHash.Reset()
		for start := 0; start < len(body); start += canonicalCheckBytes {
			if err := ctx.Err(); err != nil {
				return zero, err
			}
			part := body[start:min(start+canonicalCheckBytes, len(body))]
			chunkHash.Write(part)
			if v2 {
				fullHash.Write(part)
			}
		}
		if ContentHash("sha256:"+hex.EncodeToString(chunkHash.Sum(nil))) != h {
			return zero, ErrIntegrity
		}
		if !v2 {
			legacy = append(legacy, bytes.Clone(body))
			continue
		}
		// Normalize only one bounded tail at a time. Standard full chunks can
		// be retained directly without the extra tail-buffer copy.
		for len(body) > 0 {
			if err := ctx.Err(); err != nil {
				return zero, err
			}
			if len(tail) == 0 && len(body) >= ChunkTarget {
				retain(body[:ChunkTarget])
				body = body[ChunkTarget:]
				continue
			}
			n := min(ChunkTarget-len(tail), len(body))
			tail = append(tail, body[:n]...)
			body = body[n:]
			if len(tail) == ChunkTarget {
				retain(tail)
				tail = tail[:0]
			}
		}
	}
	if !v2 {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		raw, err := AssembleDocChunks(manifest, legacy, want)
		if err != nil {
			return zero, err
		}
		return v.Verify(ctx, want, raw)
	}
	if len(tail) > 0 {
		retain(tail)
	}
	fullHash.Write([]byte(canonicalDocSuffix))
	if ContentHash("sha256:"+hex.EncodeToString(fullHash.Sum(nil))) != want {
		return zero, ErrIntegrity
	}
	previous, havePrevious := 0, false
	var pending []canonicalEventProof
	var scratch []byte
	hasher := newCanonicalSpanHasher()
	err = owned.stream.visit(ctx, func(span canonicalSpan) error {
		h, err := hasher.sum(ctx, owned.stream, span)
		if err != nil {
			return err
		}
		key := canonicalEventKey{h, envelope.CIRVersion}
		v.mu.Lock()
		seq, found := v.events[key]
		v.mu.Unlock()
		if !found {
			// Only a cold event needs contiguous JSON for the existing exact
			// typed canonicalizer. Even one huge event is bounded by the doc
			// limit, and the scratch is reused instead of retaining all events.
			scratch = owned.stream.appendRange(scratch[:0], span)
			seq, err = v.eventWithKey(ctx, key, scratch, &pending)
			if err != nil {
				return err
			}
		}
		if havePrevious && seq < previous {
			return ErrIntegrity
		}
		previous, havePrevious = seq, true
		owned.events = append(owned.events, verifiedCanonicalEvent{span: span, hash: h, seq: seq})
		return ctx.Err()
	})
	if err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	v.remember(pending)
	return VerifiedSessionDoc{hash: want, chunks: owned}, nil
}
