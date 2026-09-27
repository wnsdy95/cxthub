package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// NewGitScanQuery exposes durable scan status without GitHub credentials,
// job submission, or background workers.
func NewGitScanQuery(core *Service) (inbound.GitScanQuery, error) {
	st, ok := core.meta.(outbound.GitScanStore)
	if !ok {
		return nil, domain.ErrValidation
	}
	return &gitScanQuery{core: core, store: st}, nil
}

type gitScanQuery struct {
	core  *Service
	store outbound.GitScanStore
}

func (g *gitScanQuery) ListScans(ctx context.Context, repo domain.ContentHash, cursor string, limit int) (domain.GitScanPage, error) {
	return repositoryRead(ctx, g.core, func(ctx context.Context) (domain.GitScanPage, error) {
		out := domain.GitScanPage{Items: []domain.GitScanJob{}}
		if domain.ValidateContentHash(repo) != nil || limit < 1 || limit > 100 {
			return out, domain.ErrValidation
		}
		if cursor != "" && domain.ValidateGitChangeID(cursor) != nil {
			return out, domain.ErrValidation
		}
		items, err := g.store.ListGitScans(ctx, repo, cursor, limit+1)
		if err != nil {
			return out, err
		}
		if len(items) > limit {
			out.NextCursor = items[limit-1].ID
			items = items[:limit]
		}
		out.Items = items
		if st, ok := g.core.meta.(outbound.GitHeadScanStore); ok {
			r, e := g.core.meta.GetRepo(ctx, repo)
			if e != nil {
				return out, e
			}
			head, e := st.GetGitHeadScan(ctx, repo, r.GitRemoteURL)
			if e == nil {
				out.Reconciliation = &head
			} else if !errors.Is(e, domain.ErrNotFound) {
				return out, e
			}
		}
		return out, nil
	})
}
