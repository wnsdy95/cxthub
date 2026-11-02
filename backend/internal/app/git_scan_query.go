package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// NewGitScanQuery exposes durable scan status without GitHub credentials,
// job submission, or background workers.
func NewGitScanQuery(core *Service) (inbound.GitScanQuery, error) {
	reader, ok := core.meta.(evidencequery.ScanReader)
	if !ok {
		return nil, domain.ErrValidation
	}
	return newGitScanQueries(core, reader)
}

type gitScanQuery struct {
	core  *Service
	store outbound.GitScanStore
}

func (g *gitScanQuery) ListScans(ctx context.Context, repo domain.ContentHash, cursor string, limit int) (domain.GitScanPage, error) {
	queries, err := newGitScanQueries(g.core, g.store)
	if err != nil {
		return domain.GitScanPage{}, err
	}
	return queries.ListScans(ctx, repo, cursor, limit)
}
