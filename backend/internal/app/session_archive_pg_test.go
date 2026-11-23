//go:build postgres

package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type archiveRevisionFailure struct{ *store.PostgresStore }

func (storage archiveRevisionFailure) AdvanceRepositoryRevision(context.Context, domain.ContentHash, bool) error {
	return errors.New("injected revision failure")
}
