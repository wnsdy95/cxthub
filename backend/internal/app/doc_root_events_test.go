package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type mismatchedRootEvents struct {
	*store.FSStore
	replacement domain.ContentHash
}

func (reader mismatchedRootEvents) ReadVerifiedRootEvents(ctx context.Context, repo, hash domain.ContentHash, selection domain.RootEventSelection) (domain.VerifiedRootEvents, error) {
	if reader.replacement == "" {
		return domain.VerifiedRootEvents{}, nil
	}
	return reader.FSStore.ReadVerifiedRootEvents(ctx, repo, reader.replacement, selection)
}

func TestRootEventPageChecksProjectionReference(test *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), test.TempDir()
	storage := store.NewFSStore(dir)
	target, _ := seedRootReadDoc(test, dir, storage, repo, historyMessage(domain.RoleUser, "requested"))
	other, _ := seedRootReadDoc(test, dir, storage, repo, historyMessage(domain.RoleUser, "other"))
	for _, replacement := range []domain.ContentHash{"", other.Hash} {
		service := NewService(storage, mismatchedRootEvents{storage, replacement}, nil, nil, nil)
		for _, base := range []domain.ContentHash{"", target.Hash} {
			page, err := service.ReadDocEvents(ctx, repo, target.Hash, base, 0, 1)
			if !errors.Is(err, domain.ErrIntegrity) || len(page.Events) != 0 {
				test.Fatal("zero or mismatched projection admitted", err)
			}
		}
	}
}
