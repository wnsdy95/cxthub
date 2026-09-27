package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// NewGitChangeQuery reads previously verified evidence without provider access
// or command methods. The API command service embeds the same query rules.
func NewGitChangeQuery(st outbound.GitChangeStore) inbound.GitChangeQuery {
	return &gitChangeQuery{store: st}
}

type gitChangeQuery struct{ store outbound.GitChangeStore }

func (g *gitChangeQuery) Get(ctx context.Context, repo domain.ContentHash, id string) (domain.GitChangeJob, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.GitChangeJob{}, err
	}
	if err := domain.ValidateGitChangeID(id); err != nil {
		return domain.GitChangeJob{}, err
	}
	return g.store.GetGitChange(ctx, repo, id)
}
func (g *gitChangeQuery) List(ctx context.Context, repo domain.ContentHash, cursor string, limit int) (domain.GitChangePage, error) {
	out := domain.GitChangePage{Items: []domain.GitChangeSummary{}}
	if err := domain.ValidateContentHash(repo); err != nil {
		return out, err
	}
	if cursor != "" {
		if err := domain.ValidateGitChangeID(cursor); err != nil {
			return out, err
		}
	}
	if limit < 1 || limit > 100 {
		return out, domain.ErrValidation
	}
	jobs, err := g.store.ListGitChanges(ctx, repo, cursor, limit+1)
	if err != nil {
		return out, err
	}
	if len(jobs) > limit {
		out.NextCursor = jobs[limit-1].ID
		jobs = jobs[:limit]
	}
	out.Items = jobs
	return out, nil
}
