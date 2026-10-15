package store

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const maxStoredRangeRootBytes = 2 * domain.MaxConversationManifestBytes
const maxStoredRangeChunkBytes = 2 * domain.ConversationManifestChunkBytes

func rangeRootManifest(ctx context.Context, hash domain.ContentHash, raw []byte) (domain.ConversationManifest, error) {
	var zero domain.ConversationManifest
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if len(raw) > maxStoredRangeRootBytes {
		return zero, domain.ErrIntegrity
	}
	reader, closeReader, err := docObjectStream(raw)
	if err != nil {
		return zero, domain.ErrIntegrity
	}
	defer closeReader()
	body, err := io.ReadAll(io.LimitReader(reader, domain.MaxConversationManifestBytes+1))
	if err != nil || len(body) > domain.MaxConversationManifestBytes {
		return zero, domain.ErrIntegrity
	}
	manifest, root, err := storedConversationManifest(ctx, body)
	if err != nil {
		return zero, err
	}
	if !root {
		return zero, domain.ErrUnsupportedDocumentIdentity
	}
	got, err := domain.ConversationManifestHash(manifest)
	if err != nil || got != hash {
		return zero, domain.ErrIntegrity
	}
	return manifest, ctx.Err()
}

func decodeRangeHeader(hash domain.ContentHash, raw []byte) (domain.DocRangeIndex, error) {
	var header domain.DocRangeIndex
	if len(raw) == 0 || len(raw) > domain.MaxDocRangeIndexBytes || json.Unmarshal(raw, &header) != nil {
		return domain.DocRangeIndex{}, domain.ErrIntegrity
	}
	if err := header.Validate(hash); err != nil {
		return domain.DocRangeIndex{}, err
	}
	return header, nil
}

func matchRangeHeader(current, supplied domain.DocRangeIndex) error {
	if err := supplied.Validate(current.Hash); err != nil {
		return err
	}
	currentRaw, err := json.Marshal(current)
	if err != nil {
		return domain.ErrIntegrity
	}
	suppliedRaw, err := json.Marshal(supplied)
	if err != nil || !bytes.Equal(currentRaw, suppliedRaw) {
		return domain.ErrIntegrity
	}
	return nil
}

func verifiedRangeHeader(index domain.VerifiedDocRangeIndex) (domain.DocRangeIndex, []byte, error) {
	header := index.Header()
	if err := header.Validate(header.Hash); err != nil {
		return domain.DocRangeIndex{}, nil, err
	}
	raw, err := json.Marshal(header)
	if err != nil || len(raw) > domain.MaxDocRangeIndexBytes {
		return domain.DocRangeIndex{}, nil, domain.ErrIntegrity
	}
	return header, raw, nil
}

func verifyStoredRangeIndex(ctx context.Context, verifier *domain.CanonicalDocVerifier, hash domain.ContentHash, manifest domain.ConversationManifest, read storedChunkReader, buffer domain.DocRangePageBuffer) (domain.VerifiedDocRangeIndex, error) {
	lengths := make(map[domain.ContentHash]int64, len(manifest.Chunks))
	for _, chunk := range manifest.Chunks {
		if prior, seen := lengths[chunk.Hash]; seen && prior != chunk.Bytes {
			return domain.VerifiedDocRangeIndex{}, domain.ErrIntegrity
		}
		lengths[chunk.Hash] = chunk.Bytes
	}
	return verifier.VerifyConversationRangeIndex(ctx, hash, manifest, func(ctx context.Context, chunk domain.ContentHash) ([]byte, error) {
		raw, err := read(ctx, chunk)
		if err != nil {
			return nil, err
		}
		return rootChunkBytes(ctx, raw, lengths[chunk])
	}, buffer)
}
