package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type mismatchedRootEvents struct {
	*store.FSStore
	replacement domain.ContentHash
}

func (reader mismatchedRootEvents) ReadVerifiedRootEvents(ctx context.Context, repo, hash domain.ContentHash, selection domain.RootEventSelection) (domain.VerifiedRootEvents, error) {
	if reader.replacement == "" {
		return domain.VerifiedRootEvents{}, nil
	}
	return reader.FSStore.ReadVerifiedRootEvents(ctx, repo, reader.replacement, selection)
}
