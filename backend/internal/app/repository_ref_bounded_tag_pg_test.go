//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type boundedTagOwnerPG struct {
	*store.PostgresStore
	lookups int
}

func (*boundedTagOwnerPG) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	panic("PostgreSQL bounded tag owner materialized a snapshot")
}
