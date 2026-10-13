//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (storage *PostgresStore) ReadVerifiedRootEvents(ctx context.Context, repo, hash domain.ContentHash, selection domain.RootEventSelection) (projection domain.VerifiedRootEvents, err error) {
	err = storage.WithinReadSnapshot(ctx, func(ctx context.Context) error {
		raw, err := storage.readOwnedDocObject(ctx, repo, "doc", hash)
		if err != nil {
			return err
		}
		manifest, root, err := storedConversationManifest(ctx, raw)
		if err != nil {
			return err
		}
		if !root {
			return domain.ErrUnsupportedDocumentIdentity
		}
		projection, err = verifyStoredConversationEvents(ctx, &storage.docProofs.rootVerifier, hash, manifest, storage.ownedDocChunkReader(repo, conversationChunkOrder(manifest)), selection)
		return err
	})
	if err != nil {
		return domain.VerifiedRootEvents{}, err
	}
	return projection, nil
}

var _ outbound.VerifiedRootEventReader = (*PostgresStore)(nil)
