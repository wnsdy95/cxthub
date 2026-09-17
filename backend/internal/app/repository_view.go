package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// GetRepositoryView pins every graph input to the same PostgreSQL MVCC snapshot.
// A failed input fails the whole view; clients keep the last complete generation.
func (s *Service) GetRepositoryView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.RepositoryView{}, err
	}
	return repositoryRead(ctx, s, func(ctx context.Context) (v domain.RepositoryView, err error) {
		if v.Refs, err = s.ListRefs(ctx, repo); err != nil {
			return
		}
		if v.Snapshots, err = s.List(ctx, inbound.ListSnapshotsInput{RepoID: repo}); err != nil {
			return
		}
		if v.Reflog, err = s.Reflog(ctx, repo); err != nil {
			return
		}
		if v.History, err = s.ListHistory(ctx, repo); err != nil {
			return
		}
		if v.Pending, err = s.ListPendings(ctx, repo); err != nil {
			return
		}
		if v.Unsync, err = s.ListUnsyncs(ctx, repo); err != nil {
			return
		}
		v.Refs = nonNil(v.Refs)
		v.Snapshots = nonNil(v.Snapshots)
		v.Reflog = nonNil(v.Reflog)
		v.History = nonNil(v.History)
		v.Pending = nonNil(v.Pending)
		v.Unsync = nonNil(v.Unsync)
		return
	})
}

func nonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}
