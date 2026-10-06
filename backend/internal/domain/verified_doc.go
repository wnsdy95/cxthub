package domain

import "encoding/json"

// VerifiedSessionDoc carries immutable canonical content after schema and hash
// validation. Its private strings cannot alias caller-owned CIR or byte slices.
// This is a request-scoped value, never a persisted trust flag or wire input.
type VerifiedSessionDoc struct {
	hash      ContentHash
	canonical string
	chunks    *verifiedDocChunks
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
	return d.hash != "" && (d.canonical != "" || d.chunks != nil)
}
func (d VerifiedSessionDoc) Hash() ContentHash { return d.hash }

// Bytes materializes an owned canonical document for legacy consumers. Chunk
// storage and read-index planning do not need this cumulative allocation.
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

// ChunkPlan exports owned v2 chunks at the standard ChunkTarget boundaries,
// regardless of the partition supplied to VerifyChunks. This is important for
// readers that derive chunk numbers from byte offsets. Empty/invalid documents
// have no chunk plan, as with PlanDocChunks. Raw and v1 values are split lazily.
func (d VerifiedSessionDoc) ChunkPlan() (DocChunkPlan, bool) {
	if !d.Valid() {
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
