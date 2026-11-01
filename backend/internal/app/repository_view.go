package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (service *Service) GetRepositoryView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.RepositoryView{}, err
	}
	queries, err := service.GraphQueries()
	if err != nil {
		return domain.RepositoryView{}, err
	}
	return queries.GetRepositoryView(ctx, repo)
}

func (service *Service) loadRepositoryView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	history, _ := service.meta.(repositoryquery.HistoryReader)
	revisions, _ := service.meta.(repositoryquery.RevisionReader)
	archives, _ := service.meta.(repositoryquery.ArchiveReader)
	return repositoryquery.LoadRepositoryView(ctx, repo, repositoryquery.RepositoryViewReaders{Metadata: service.meta, History: history, Revisions: revisions, Archives: archives})
}

func nonNil[Value any](items []Value) []Value {
	if items == nil {
		return []Value{}
	}
	return items
}

func (service *Service) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	queries, err := service.RepositoryStateQueries()
	if err != nil {
		return domain.RepositoryRevision{}, err
	}
	return queries.RepositoryRevision(ctx, repo)
}

func (service *Service) GetPendingView(ctx context.Context, repo domain.ContentHash) (domain.PendingView, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.PendingView{}, err
	}
	queries, err := service.GraphQueries()
	if err != nil {
		return domain.PendingView{}, err
	}
	return queries.GetPendingView(ctx, repo)
}

func pendingViewFromRepository(base domain.RepositoryView) (domain.PendingView, error) {
	return domain.ProjectPendingView(base)
}
