package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type missingMemoryBlob struct {
	*store.FSStore
}

func (s *missingMemoryBlob) GetMemory(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
	return domain.MemoryDigest{}, domain.ErrNotFound
}
