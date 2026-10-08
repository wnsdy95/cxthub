package domain

import (
	"context"
	"encoding/json"
)

// VerifiedSessionDoc carries immutable canonical content after schema and hash
// validation. Its private strings cannot alias caller-owned CIR or byte slices.
// This is a request-scoped value, never a persisted trust flag or wire input.
type VerifiedSessionDoc struct {
	hash      ContentHash
	canonical string
	chunks    *verifiedDocChunks
	identity  DocumentIdentity
	// Root identity commits this exact canonical manifest, not Bytes().
	rootManifest string
}

func VerifySessionDoc(doc SessionDoc) (VerifiedSessionDoc, error) {
	raw, err := ValidatedSessionDocBytes(doc)
	if err != nil {
		return VerifiedSessionDoc{}, err
	}
	// CanonicalBytes normalizes each event independently. Include both JSON
	// wrapper levels in the decoder's nesting limit before publishing a proof,
	// so a typed value cannot produce an archive the normal reader cannot decode.
	if !json.Valid(raw) {
		return VerifiedSessionDoc{}, ErrIntegrity
	}
	return VerifiedSessionDoc{hash: doc.Hash, canonical: string(raw)}, nil
}

func (d VerifiedSessionDoc) Valid() bool {
	if d.hash == "" {
		return false
	}
	switch d.identity {
	case DocumentIdentityLegacy:
		return d.rootManifest == "" && (d.canonical != "" || d.chunks != nil)
	case DocumentIdentityRootV1:
		return d.rootManifest != "" && d.chunks != nil && d.canonical == ""
	default:
		return false
	}
}
func (d VerifiedSessionDoc) Hash() ContentHash { return d.hash }

func (d VerifiedSessionDoc) DocumentRef() DocumentRef {
	return DocumentRef{Hash: d.hash, Identity: d.identity}
}

// ConversationManifest returns an owned copy of the exact verified root
// manifest. Legacy and invalid proofs have no root manifest.
func (d VerifiedSessionDoc) ConversationManifest() (ConversationManifest, bool) {
	if !d.Valid() || d.identity != DocumentIdentityRootV1 {
		return ConversationManifest{}, false
	}
	// Private immutable bytes were validated before this proof was created.
	manifest, err := DecodeConversationManifest([]byte(d.rootManifest))
	return manifest, err == nil
}

// Bytes materializes owned canonical CIR under either identity. For a root,
// SHA256(Bytes()) is NOT Hash(). Chunk-backed reads and index planning do not
// need this cumulative allocation.
func (d VerifiedSessionDoc) Bytes() []byte {
	if d.chunks == nil {
		return []byte(d.canonical)
	}
	c := d.chunks
	out := make([]byte, 0, len(c.envelope)+canonicalFrameBytes+c.stream.size)
	out = append(out, canonicalDocPrefix...)
	out = append(out, c.envelope...)
	out = append(out, canonicalDocMiddle...)
	for _, part := range c.stream.parts {
		out = append(out, part...)
	}
	return append(out, canonicalDocSuffix...)
}

// EventStreamRange copies bytes from the canonical events-array interior of
// this proof, never from current storage or caller-owned slices. A zero-length
// range at the end is valid, including for an empty root.
func (d VerifiedSessionDoc) EventStreamRange(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !d.Valid() {
		return nil, ErrIntegrity
	}
	_, stream, err := d.segmented()
	if err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || offset > stream.size || length > stream.size-offset {
		return nil, ErrIntegrity
	}
	out := make([]byte, 0, length)
	stream.ranges(canonicalSpan{offset, length}, func(part string) {
		for len(part) > 0 && err == nil {
			if err = ctx.Err(); err != nil {
				return
			}
			n := min(len(part), canonicalCheckBytes)
			out = append(out, part[:n]...)
			part = part[n:]
		}
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ChunkPlan exports legacy owned v2 chunks at the standard ChunkTarget boundaries,
// regardless of the partition supplied to VerifyChunks. This is important for
// readers that derive chunk numbers from byte offsets. Empty/invalid documents
// have no chunk plan, as with PlanDocChunks. Raw and v1 values are split lazily.
// Root proofs never export a lossy legacy descriptor. Publication callers must
// reject unsupported identities before interpreting false as whole-CIR fallback.
func (d VerifiedSessionDoc) ChunkPlan() (DocChunkPlan, bool) {
	if !d.Valid() || d.identity != DocumentIdentityLegacy {
		return DocChunkPlan{}, false
	}
	env, stream, err := d.segmented()
	if err != nil || stream.size == 0 {
		return DocChunkPlan{}, false
	}
	p := DocChunkPlan{Manifest: DocChunkManifest{Format: ChunkFormatV2, Envelope: []byte(env)}, Bodies: make(map[ContentHash][]byte)}
	for offset := 0; offset < stream.size; offset += ChunkTarget {
		body := stream.appendRange(nil, canonicalSpan{offset, min(ChunkTarget, stream.size-offset)})
		var h ContentHash
		if d.chunks != nil {
			h = d.chunks.hashes[offset/ChunkTarget]
		} else {
			h = HashContent(body)
		}
		p.Manifest.Chunks = append(p.Manifest.Chunks, h)
		p.Order = append(p.Order, h)
		p.Bodies[h] = body
	}
	return p, true
}
