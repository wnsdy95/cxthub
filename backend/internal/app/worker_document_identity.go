package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Worker declarations describe this binary, not the caller's requested support.
// Preserve the actor: only existing trusted entry points add a system actor.
func (s *Service) workerDocumentIdentityContext(ctx context.Context) context.Context {
	ids := s.DocumentIdentitiesSupported()
	return outbound.WithDocumentIdentityCompatibility(s.DocumentIdentityWorkerContext(ctx), ids, ids)
}

func (s *Service) workerRepository(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return repositoryReadForRepo(ctx, s, repo, func(tx context.Context) (domain.Repo, error) {
		return s.meta.GetRepo(tx, repo)
	})
}
