package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *Service) QueryContext(ctx context.Context, repo domain.ContentHash, in domain.ContextSelection) (domain.ContextQueryView, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.ContextQueryView{}, err
	}
	return repositoryRead(ctx, s, func(ctx context.Context) (domain.ContextQueryView, error) {
		r, err := s.meta.GetRepo(ctx, repo)
		if err != nil {
			return domain.ContextQueryView{}, err
		}
		view, err := s.loadRepositoryView(ctx, repo)
		if err != nil {
			return domain.ContextQueryView{}, err
		}
		branch := r.DefaultBranch
		if branch == "" {
			branch = "main"
		}
		return domain.SelectContext(view, in, branch)
	})
}
