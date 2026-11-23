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

func (s *rootReadSpy) MatchingDocHashes(context.Context, domain.ContentHash, string) (map[domain.ContentHash]bool, error) {
	// A root without a persisted search index must still be searched.
	return map[domain.ContentHash]bool{}, nil
}
