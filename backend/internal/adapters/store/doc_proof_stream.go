package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyProofChunkSpan struct {
	offset         int
	length         int
	representation domain.ContentHash
}

type legacyProofStreamSink struct {
	size int
}

func (sink *legacyProofStreamSink) Append(ctx context.Context, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(body) > domain.MaxFinalizedDocBytes-sink.size {
		return domain.ErrDocumentResourceLimit
	}
	sink.size += len(body)
	return ctx.Err()
}

func (*legacyProofStreamSink) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, domain.ErrIntegrity
}

func (cache *docProofCache) verifyStoredLegacyStream(ctx context.Context, repo, want, storedHash domain.ContentHash, manifest domain.DocChunkManifest, read storedChunkReader) (domain.VerifiedDocReference, error) {
	var zero domain.VerifiedDocReference
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if domain.ValidateContentHash(want) != nil || manifest.Format != domain.ChunkFormatV2 || (read == nil && len(manifest.Chunks) > 0) ||
		len(manifest.Chunks) > domain.MaxDocJobChunks || len(manifest.Envelope) > domain.MaxDocJobManifestBytes {
		return zero, domain.ErrIntegrity
	}
	encoded, err := json.Marshal(manifest)
	if err != nil || len(encoded) > domain.MaxDocJobManifestBytes {
		return zero, domain.ErrIntegrity
	}
	physical, err := newLegacyDocSpool()
	if err != nil {
		return zero, err
	}
	defer physical.Close()
	fingerprint := sha256.New()
	fingerprint.Write([]byte("cxt-stored-doc-proof-v1\x00"))
	fingerprint.Write([]byte(storedHash))
	spans := make(map[domain.ContentHash]legacyProofChunkSpan, len(manifest.Chunks))
	for _, hash := range manifest.Chunks {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if validateHash(hash) != nil {
			return zero, domain.ErrIntegrity
		}
		if _, seen := spans[hash]; seen {
			continue
		}
		raw, err := read(ctx, hash)
		if err != nil {
			return zero, err
		}
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if len(raw) > 2*maxLegacyReadBytes || len(raw) > domain.MaxFinalizedDocBytes-physical.size {
			return zero, domain.ErrDocumentResourceLimit
		}
		representation := domain.HashContent(raw)
		fingerprint.Write([]byte(hash))
		fingerprint.Write([]byte(representation))
		span := legacyProofChunkSpan{offset: physical.size, length: len(raw), representation: representation}
		if err := physical.Append(ctx, raw); err != nil {
			return zero, err
		}
		spans[hash] = span
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	key := docProofKey{repo: repo, expected: want, representation: domain.ContentHash("sha256:" + hex.EncodeToString(fingerprint.Sum(nil)))}
	if proof, ok := cache.get(key); ok {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		return proof, nil
	}
	verified, err := cache.rootVerifier.VerifyLegacyStream(ctx, want, manifest, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		span, ok := spans[hash]
		if !ok {
			return nil, domain.ErrIntegrity
		}
		raw, err := readLegacyProofChunk(ctx, physical, span)
		if err != nil {
			return nil, err
		}
		return boundedLegacyObject(ctx, raw, maxLegacyReadBytes)
	}, &legacyProofStreamSink{})
	if err != nil {
		if errors.Is(err, domain.ErrUnsupportedCIRVersion) {
			return zero, domain.ErrIntegrity
		}
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	proof := verified.Reference()
	cache.put(key, proof)
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return proof, nil
}

func readLegacyProofChunk(ctx context.Context, spool *legacyDocSpool, span legacyProofChunkSpan) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if span.offset < 0 || span.length < 0 || span.offset > spool.size || span.length > spool.size-span.offset {
		return nil, domain.ErrIntegrity
	}
	if span.length > 2*maxLegacyReadBytes {
		return nil, domain.ErrDocumentResourceLimit
	}
	raw := make([]byte, span.length)
	for offset := 0; offset < len(raw); offset += domain.ChunkTarget {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := spool.file.ReadAt(raw[offset:min(offset+domain.ChunkTarget, len(raw))], int64(span.offset+offset)); err != nil {
			return nil, err
		}
	}
	if domain.HashContent(raw) != span.representation {
		return nil, domain.ErrIntegrity
	}
	return raw, ctx.Err()
}
