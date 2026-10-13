package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func verifyStoredConversationReference(ctx context.Context, verifier *domain.CanonicalDocVerifier, hash domain.ContentHash, manifest domain.ConversationManifest, read storedChunkReader) (domain.VerifiedDocReference, error) {
	lengths := make(map[domain.ContentHash]int64, len(manifest.Chunks))
	for _, chunk := range manifest.Chunks {
		if previous, seen := lengths[chunk.Hash]; seen && previous != chunk.Bytes {
			return domain.VerifiedDocReference{}, domain.ErrIntegrity
		}
		lengths[chunk.Hash] = chunk.Bytes
	}
	return verifier.VerifyConversationManifestReference(ctx, hash, manifest, func(ctx context.Context, chunk domain.ContentHash) ([]byte, error) {
		raw, err := read(ctx, chunk)
		if err != nil {
			return nil, err
		}
		return rootChunkBytes(ctx, raw, lengths[chunk])
	})
}
