package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type prDiagnosticFailureStore struct {
	*store.FSStore
	failure error
}

func (s *prDiagnosticFailureStore) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return domain.Repo{}, s.failure
}
