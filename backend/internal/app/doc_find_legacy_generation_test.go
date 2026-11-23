package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyFindGenerationMeta struct {
	*store.FSStore
	snapshot domain.Snapshot
	failure  error
}

func (metadata *legacyFindGenerationMeta) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	return metadata.snapshot, metadata.failure
}
