package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rootReadSpy struct {
	*store.FSStore
	reads int
	after func()
}

func (s *rootReadSpy) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	s.reads++
	doc, err := s.FSStore.ReadVerifiedDoc(ctx, repo, hash)
	if err == nil && s.after != nil {
		s.after()
	}
	return doc, err
}

func (spy *rootReadSpy) ReadVerifiedRootEvents(ctx context.Context, repo, hash domain.ContentHash, selection domain.RootEventSelection) (domain.VerifiedRootEvents, error) {
	spy.reads++
	projection, err := spy.FSStore.ReadVerifiedRootEvents(ctx, repo, hash, selection)
	if err == nil && spy.after != nil {
		spy.after()
	}
	return projection, err
}

func (s *rootReadSpy) MatchingDocHashes(context.Context, domain.ContentHash, string) (map[domain.ContentHash]bool, error) {
	// A root without a persisted search index must still be searched.
	return map[domain.ContentHash]bool{}, nil
}
