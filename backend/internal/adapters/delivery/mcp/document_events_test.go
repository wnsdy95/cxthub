package mcp

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type documentEventQueriesProbe struct {
	err       error
	read      func(context.Context, domain.ContentHash) error
	events    int
	fragments int
	searches  int
}

func (queries *documentEventQueriesProbe) ReadDocFragments(ctx context.Context, repo, hash domain.ContentHash, index, offset, limit, budget int) (domain.DocFragmentPage, error) {
	queries.fragments++
	if queries.read != nil {
		return domain.DocFragmentPage{}, queries.read(ctx, repo)
	}
	return domain.DocFragmentPage{}, queries.err
}

func (queries *documentEventQueriesProbe) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, query string, after, limit int) ([]domain.DocEventIndex, error) {
	queries.searches++
	if queries.read != nil {
		return nil, queries.read(ctx, repo)
	}
	return nil, queries.err
}

type documentEventBackendTrap struct {
	pageBackend
	documentEventQueriesProbe
	whole int
}

func (backend *documentEventBackendTrap) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	backend.whole++
	return domain.SessionDoc{}, domain.ErrIntegrity
}
