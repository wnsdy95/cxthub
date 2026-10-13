package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func verifyStoredConversationEvents(ctx context.Context, verifier *domain.CanonicalDocVerifier, hash domain.ContentHash, manifest domain.ConversationManifest, read storedChunkReader, selection domain.RootEventSelection) (domain.VerifiedRootEvents, error) {
	lengths := make(map[domain.ContentHash]int64, len(manifest.Chunks))
	for _, chunk := range manifest.Chunks {
		if previous, seen := lengths[chunk.Hash]; seen && previous != chunk.Bytes {
			return domain.VerifiedRootEvents{}, domain.ErrIntegrity
		}
		lengths[chunk.Hash] = chunk.Bytes
	}
	return verifier.VerifyConversationEvents(ctx, hash, manifest, func(ctx context.Context, chunk domain.ContentHash) ([]byte, error) {
		raw, err := read(ctx, chunk)
		if err != nil {
			return nil, err
		}
		return rootChunkBytes(ctx, raw, lengths[chunk])
	}, selection)
}

func (storage *FSStore) ReadVerifiedRootEvents(ctx context.Context, repo, hash domain.ContentHash, selection domain.RootEventSelection) (domain.VerifiedRootEvents, error) {
	raw, err := storage.readDocObject(ctx, repo, hash)
	if err != nil {
		return domain.VerifiedRootEvents{}, err
	}
	manifest, root, err := storedConversationManifest(ctx, raw)
	if err != nil {
		return domain.VerifiedRootEvents{}, err
	}
	if !root {
		return domain.VerifiedRootEvents{}, domain.ErrUnsupportedDocumentIdentity
	}
	return verifyStoredConversationEvents(ctx, &storage.docProofs.rootVerifier, hash, manifest, storage.ownedFSChunkReader(repo), selection)
}

var _ outbound.VerifiedRootEventReader = (*FSStore)(nil)
