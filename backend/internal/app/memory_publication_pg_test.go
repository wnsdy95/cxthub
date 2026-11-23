//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rejectPublicationMemoryPG struct{ *store.PostgresStore }

func (s *rejectPublicationMemoryPG) CompareAndSwapSnapshotMemory(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, domain.ContentHash) error {
	return domain.ErrIntegrity
}
