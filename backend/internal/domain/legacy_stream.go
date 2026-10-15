package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const MaxLegacyStreamEventBytes = 32 << 20

var ErrDocumentResourceLimit = errors.New("document resource limit exceeded")

type VerifiedDocStream interface {
	Append(context.Context, []byte) error
	Read(context.Context, int, int) ([]byte, error)
}

type VerifiedLegacyStream struct {
	reference VerifiedDocReference
	manifest  DocChunkManifest
	source    VerifiedDocStream
	size      int
	events    []verifiedCanonicalEvent
}

func (doc VerifiedLegacyStream) Valid() bool {
	return doc.reference.Valid() && doc.manifest.Format == ChunkFormatV2 && doc.source != nil
}

func (doc VerifiedLegacyStream) Reference() VerifiedDocReference {
	if !doc.Valid() {
		return VerifiedDocReference{}
	}
	return doc.reference
}

func (doc VerifiedLegacyStream) Manifest() DocChunkManifest {
	return DocChunkManifest{Format: doc.manifest.Format, Envelope: bytes.Clone(doc.manifest.Envelope), Chunks: append([]ContentHash(nil), doc.manifest.Chunks...)}
}

func (doc VerifiedLegacyStream) Chunk(ctx context.Context, ordinal int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !doc.Valid() || ordinal < 0 || ordinal >= len(doc.manifest.Chunks) {
		return nil, ErrIntegrity
	}
	offset := ordinal * ChunkTarget
	length := min(ChunkTarget, doc.size-offset)
	current, err := doc.source.Read(ctx, offset, length)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(current) != length {
		return nil, ErrIntegrity
	}
	body := bytes.Clone(current)
	hash, err := legacyStreamHash(ctx, body)
	if err != nil {
		return nil, err
	}
	if hash != doc.manifest.Chunks[ordinal] {
		return nil, ErrIntegrity
	}
	return body, nil
}

func (doc VerifiedLegacyStream) PlanReadIndexContext(ctx context.Context) (DocReadPlan, error) {
	if err := ctx.Err(); err != nil {
		return DocReadPlan{}, err
	}
	if !doc.Valid() {
		return DocReadPlan{}, ErrIntegrity
	}
	index := DocReadIndex{Version: 1, Hash: doc.reference.Hash(), Events: make([]DocEventIndex, 0, len(doc.events))}
	if err := json.Unmarshal(doc.manifest.Envelope, &index.Envelope); err != nil {
		return DocReadPlan{}, err
	}
	spans := make([]canonicalSpan, 0, len(doc.events))
	for ordinal, event := range doc.events {
		if err := ctx.Err(); err != nil {
			return DocReadPlan{}, err
		}
		index.Events = append(index.Events, DocEventIndex{Index: ordinal, Offset: event.span.offset, Length: event.span.length, Hash: event.hash, Seq: event.seq})
		spans = append(spans, event.span)
	}
	if err := ctx.Err(); err != nil {
		return DocReadPlan{}, err
	}
	return DocReadPlan{index: index, source: doc.source, bodies: spans, sequencesKnown: true}, nil
}

func (verifier *CanonicalDocVerifier) VerifyLegacyStream(ctx context.Context, want ContentHash, manifest DocChunkManifest, load func(context.Context, ContentHash) ([]byte, error), stream VerifiedDocStream) (VerifiedLegacyStream, error) {
	return verifier.verifyLegacyStream(ctx, want, manifest, load, stream, MaxLegacyStreamEventBytes, MaxFinalizedDocBytes)
}

func (verifier *CanonicalDocVerifier) verifyLegacyStream(ctx context.Context, want ContentHash, manifest DocChunkManifest, load func(context.Context, ContentHash) ([]byte, error), stream VerifiedDocStream, eventLimit, documentLimit int) (VerifiedLegacyStream, error) {
	var zero VerifiedLegacyStream
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if ValidateContentHash(want) != nil || manifest.Format != ChunkFormatV2 || stream == nil || (load == nil && len(manifest.Chunks) > 0) ||
		len(manifest.Chunks) > MaxDocJobChunks || len(manifest.Envelope) == 0 || len(manifest.Envelope) > MaxDocJobManifestBytes {
		return zero, ErrIntegrity
	}
	manifest.Envelope = bytes.Clone(manifest.Envelope)
	manifest.Chunks = append([]ContentHash(nil), manifest.Chunks...)
	for _, hash := range manifest.Chunks {
		if ValidateContentHash(hash) != nil {
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
	total := canonicalFrameBytes + len(manifest.Envelope)
	if total > documentLimit {
		return zero, ErrDocumentResourceLimit
	}
	fullHash, chunkHash, normalizedHash := sha256.New(), sha256.New(), sha256.New()
	fullHash.Write([]byte(canonicalDocPrefix))
	fullHash.Write(manifest.Envelope)
	fullHash.Write([]byte(canonicalDocMiddle))
	result := VerifiedLegacyStream{source: stream, manifest: DocChunkManifest{Format: ChunkFormatV2, Envelope: manifest.Envelope}}
	var pending []canonicalEventProof
	scanner := legacyStreamScanner{limit: eventLimit, visit: func(span canonicalSpan, body []byte) (int, error) {
		hash, err := legacyStreamHash(ctx, body)
		if err != nil {
			return 0, err
		}
		sequence, err := verifier.eventWithKey(ctx, canonicalEventKey{hash: hash, version: envelope.CIRVersion}, body, &pending)
		if err != nil {
			return 0, err
		}
		result.events = append(result.events, verifiedCanonicalEvent{span: span, hash: hash, seq: sequence})
		return sequence, nil
	}}
	normalizedBytes := 0
	work := make([]byte, canonicalCheckBytes)
	for _, hash := range manifest.Chunks {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		body, err := load(ctx, hash)
		if err != nil {
			return zero, err
		}
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if len(body) > documentLimit-total {
			return zero, ErrDocumentResourceLimit
		}
		total += len(body)
		chunkHash.Reset()
		for offset := 0; offset < len(body); {
			if err := ctx.Err(); err != nil {
				return zero, err
			}
			length := min(len(work), len(body)-offset, ChunkTarget-normalizedBytes)
			part := work[:length]
			copy(part, body[offset:offset+length])
			chunkHash.Write(part)
			fullHash.Write(part)
			normalizedHash.Write(part)
			if err := scanner.add(ctx, part); err != nil {
				return zero, err
			}
			normalizedBytes += length
			if normalizedBytes == ChunkTarget {
				result.manifest.Chunks = append(result.manifest.Chunks, ContentHash("sha256:"+hex.EncodeToString(normalizedHash.Sum(nil))))
				normalizedHash.Reset()
				normalizedBytes = 0
			}
			if err := stream.Append(ctx, part); err != nil {
				return zero, err
			}
			offset += length
		}
		if ContentHash("sha256:"+hex.EncodeToString(chunkHash.Sum(nil))) != hash {
			return zero, ErrIntegrity
		}
	}
	if err := scanner.finish(ctx); err != nil {
		return zero, err
	}
	fullHash.Write([]byte(canonicalDocSuffix))
	if ContentHash("sha256:"+hex.EncodeToString(fullHash.Sum(nil))) != want {
		return zero, ErrIntegrity
	}
	if normalizedBytes > 0 {
		result.manifest.Chunks = append(result.manifest.Chunks, ContentHash("sha256:"+hex.EncodeToString(normalizedHash.Sum(nil))))
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	verifier.remember(pending)
	result.size = total - canonicalFrameBytes - len(manifest.Envelope)
	result.reference = VerifiedDocReference{hash: want, identity: DocumentIdentityLegacy}
	return result, nil
}

func legacyStreamHash(ctx context.Context, body []byte) (ContentHash, error) {
	hasher := sha256.New()
	for offset := 0; offset < len(body); offset += canonicalCheckBytes {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		hasher.Write(body[offset:min(offset+canonicalCheckBytes, len(body))])
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return ContentHash("sha256:" + hex.EncodeToString(hasher.Sum(nil))), nil
}
