package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *Service) workerRepository(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return repositoryReadForRepo(ctx, s, repo, func(tx context.Context) (domain.Repo, error) {
		return s.meta.GetRepo(tx, repo)
	})
}
