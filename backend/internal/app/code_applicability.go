package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// All deliveries use this query. No provider I/O or writes happen in the read
// transaction. Missing evidence is explicit and ordinary observations fill it.
func (s *Service) QueryCodeApplicability(ctx context.Context, repo domain.ContentHash, in domain.CodeSelection) (domain.CodeApplicability, error) {
	queries, err := s.CodeQueries()
	if err != nil {
		return domain.CodeApplicability{}, err
	}
	return queries.QueryCodeApplicability(ctx, repo, in)
}

func (evidence *codeEvidence) query(ctx context.Context, in domain.CodeSelection, revision domain.RepositoryRevision) (domain.CodeApplicability, error) {
	owner := evidence.git
	if owner == nil && evidence.trees != nil && evidence.deltas != nil && evidence.origin != "" {
		err := ctx.Err()
		if err == nil {
			owner, err = evidence.gitEvidence()
		}
		if err != nil {
			return domain.CodeApplicability{Selection: in, Revision: revision, Relation: "unknown", Paths: []domain.CodePathState{}}, err
		}
	}
	return owner.Query(ctx, in, revision)
}

func cachedGitEntry(ctx context.Context, st outbound.GitTreeStore, repo domain.ContentHash, origin, root, path string, cache map[string]domain.GitTreeNode) (domain.GitEntry, error) {
	return evidencequery.CachedGitEntry(ctx, st, repo, origin, root, path, cache)
}
