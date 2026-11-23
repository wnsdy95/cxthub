//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type unsupportedToolProjectionWorker struct {
	*store.PostgresStore
}

func (*unsupportedToolProjectionWorker) LoadDocEventDisplay(context.Context, domain.DocDisplayJob) (domain.DocEventDisplayPreparation, error) {
	return domain.DocEventDisplayPreparation{}, domain.ErrDocFindDisplayUnavailable
}
