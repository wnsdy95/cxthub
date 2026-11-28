package store

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const maxStoredRangeRootBytes = 2 * domain.MaxConversationManifestBytes
const maxStoredRangeChunkBytes = 2 * domain.ConversationManifestChunkBytes

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
