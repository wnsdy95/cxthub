package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// NewGitChangeQuery reads previously verified evidence without provider access
// or command methods. The API command service embeds the same query rules.
func NewGitChangeQuery(reader evidencequery.ChangeReader) inbound.GitChangeQuery {
	metadata, ok := reader.(evidencequery.MetadataReader)
	if !ok || !rootAdapterPresent(metadata) {
		return unavailableGitChangeQuery{failure: domain.ErrDocumentIdentityUpgradeRequired}
	}
	transactions, _ := reader.(documentQuerySnapshots)
	scope := &statusQueryScope{repository: &repositoryQueryScope{
		metadata: metadata, transactions: transactions, supported: []domain.DocumentIdentity{domain.DocumentIdentityLegacy},
	}}
	queries, err := evidencequery.NewChangeQueries(evidencequery.ChangeConfig{
		Scope: scope, Changes: reader, AtomicRead: transactions != nil,
	})
	if err != nil {
		return unavailableGitChangeQuery{failure: err}
	}
	return queries
}

type unavailableGitChangeQuery struct {
	failure error
}

func (query unavailableGitChangeQuery) Get(context.Context, domain.ContentHash, string) (domain.GitChangeJob, error) {
	return domain.GitChangeJob{}, query.failure
}

func (query unavailableGitChangeQuery) List(context.Context, domain.ContentHash, string, int) (domain.GitChangePage, error) {
	return domain.GitChangePage{}, query.failure
}

type gitChangeQuery struct {
	store outbound.GitChangeStore
	core  *Service
}

func (g *gitChangeQuery) Get(ctx context.Context, repo domain.ContentHash, id string) (domain.GitChangeJob, error) {
	if g.core == nil {
		var zero domain.GitChangeJob
		return zero, domain.ErrDocumentIdentityUpgradeRequired
	}
	queries, err := newGitChangeQueries(g.core, g.store)
	if err != nil {
		return domain.GitChangeJob{}, err
	}
	return queries.Get(ctx, repo, id)
}
func (g *gitChangeQuery) List(ctx context.Context, repo domain.ContentHash, cursor string, limit int) (domain.GitChangePage, error) {
	if g.core == nil {
		var zero domain.GitChangePage
		return zero, domain.ErrDocumentIdentityUpgradeRequired
	}
	queries, err := newGitChangeQueries(g.core, g.store)
	if err != nil {
		return domain.GitChangePage{}, err
	}
	return queries.List(ctx, repo, cursor, limit)
}
