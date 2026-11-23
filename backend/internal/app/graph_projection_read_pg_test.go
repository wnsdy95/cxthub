//go:build postgres

package app

import (
	"context"
	"sync/atomic"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphBundleReadPG struct {
	*store.PostgresStore
	liveReads int
	bodyReads atomic.Int64
}

func (storage *graphBundleReadPG) ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error) {
	storage.liveReads++
	return nil, domain.ErrIntegrity
}
