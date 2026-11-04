package repositoryquery

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type GraphConfig struct {
	Legacy         inbound.RepositoryGraphQueries
	Published      inbound.RepositoryGraphQueries
	PublishedRepos []domain.ContentHash
}

type GraphQueries struct {
	legacy    inbound.RepositoryGraphQueries
	published inbound.RepositoryGraphQueries
	enabled   map[domain.ContentHash]bool
}

var _ inbound.RepositoryGraphQueries = (*GraphQueries)(nil)

func NewGraph(config GraphConfig) (*GraphQueries, error) {
	if absent(config.Legacy) || config.Published != nil && absent(config.Published) {
		return nil, fmt.Errorf("%w: graph query dependency is required", domain.ErrCatalogUnsupported)
	}
	if len(config.PublishedRepos) > 0 && config.Published == nil {
		return nil, fmt.Errorf("%w: published graph queries are required", domain.ErrCatalogUnsupported)
	}
	queries := &GraphQueries{legacy: config.Legacy, published: config.Published, enabled: make(map[domain.ContentHash]bool, len(config.PublishedRepos))}
	for _, repo := range config.PublishedRepos {
		if err := domain.ValidateContentHash(repo); err != nil {
			return nil, err
		}
		queries.enabled[repo] = true
	}
	return queries, nil
}

func (queries *GraphQueries) GetRepositoryView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.RepositoryView{}, err
	}
	if queries.enabled[repo] {
		return queries.published.GetRepositoryView(ctx, repo)
	}
	return queries.legacy.GetRepositoryView(ctx, repo)
}

func (queries *GraphQueries) GetPendingView(ctx context.Context, repo domain.ContentHash) (domain.PendingView, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.PendingView{}, err
	}
	if queries.enabled[repo] {
		return queries.published.GetPendingView(ctx, repo)
	}
	return queries.legacy.GetPendingView(ctx, repo)
}

func (queries *GraphQueries) QueryGraphState(ctx context.Context, repo domain.ContentHash, position string) (domain.GraphState, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.GraphState{}, err
	}
	if queries.enabled[repo] {
		return queries.published.QueryGraphState(ctx, repo, position)
	}
	return queries.legacy.QueryGraphState(ctx, repo, position)
}
