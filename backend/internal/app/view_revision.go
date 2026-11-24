package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// ViewRevision returns the mark an open page follows. It is made of the
// repository's revision counters, which every write that changes what the
// Sessions tab shows moves in its own transaction: a page that reads the
// lists after it saw the mark move reads what moved it. The mark says that
// something changed, never what, and only whether two marks are the same
// means anything.
func (s *Service) ViewRevision(ctx context.Context, repo domain.ContentHash) (string, error) {
	revisions, ok := s.meta.(outbound.RepositoryRevisions)
	if !ok {
		return "", fmt.Errorf("%w: repository revision unavailable", domain.ErrConflict)
	}
	return repositoryRead(ctx, s, func(bound context.Context) (string, error) {
		// The store refuses what is no repository identifier and says when
		// it holds no such repository.
		if _, err := s.meta.GetRepo(bound, repo); err != nil {
			return "", err
		}
		revision, err := revisions.RepositoryRevision(bound, repo)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d.%d.%d", revision.Graph, revision.Pending, revision.Evidence), nil
	})
}

var _ inbound.ViewRevisions = (*Service)(nil)
