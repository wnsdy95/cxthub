package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type contextWriteReadSpy struct {
	*store.FSStore
	protocol                      int
	repoReads, historyReads, refs int
	repoErr, historyErr, refErr   error
}

func (s *contextWriteReadSpy) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	s.repoReads++
	if s.repoErr != nil {
		return domain.Repo{}, s.repoErr
	}
	r, err := s.FSStore.GetRepo(ctx, repo)
	r.ContextProtocol = s.protocol
	return r, err
}

func (s *contextWriteReadSpy) GetRef(ctx context.Context, repo domain.ContentHash, kind domain.RefKind, name string) (domain.Ref, error) {
	s.refs++
	if s.refErr != nil {
		return domain.Ref{}, s.refErr
	}
	return s.FSStore.GetRef(ctx, repo, kind, name)
}
