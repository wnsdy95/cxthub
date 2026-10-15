package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Service) GetPendingState(ctx context.Context, repo domain.ContentHash) (domain.PendingStateView, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.PendingStateView{}, err
	}
	ctx, release, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		return domain.PendingStateView{}, err
	}
	defer release()
	return repositoryReadForRepo(ctx, s, repo, func(ctx context.Context) (domain.PendingStateView, error) {
		var view domain.RepositoryView
		metadata, err := s.meta.GetRepo(ctx, repo)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return domain.PendingStateView{}, err
		}
		view.DefaultBranch = metadata.DefaultBranch
		if view.Refs, err = s.meta.ListRefs(ctx, repo); err != nil {
			return domain.PendingStateView{}, err
		}
		if view.Snapshots, err = s.meta.ListSnapshots(ctx, repo, ""); err != nil {
			return domain.PendingStateView{}, err
		}
		if view.Reflog, err = s.meta.ReadReflog(ctx, repo); err != nil {
			return domain.PendingStateView{}, err
		}
		if view.History, err = s.ListHistory(ctx, repo); err != nil {
			return domain.PendingStateView{}, err
		}
		if view.Pending, err = s.meta.ListPendings(ctx, repo); err != nil {
			return domain.PendingStateView{}, err
		}
		if view.Unsync, err = s.meta.ListUnsyncs(ctx, repo); err != nil {
			return domain.PendingStateView{}, err
		}
		archives, err := s.sessionArchiveRecords(ctx, repo)
		if err != nil {
			return domain.PendingStateView{}, err
		}
		view.ArchivedSessions = domain.ProjectSessionArchives(archives, view.Snapshots, view.History, view.DefaultBranch)
		if view.Revision, err = s.RepositoryRevision(ctx, repo); err != nil {
			return domain.PendingStateView{}, err
		}
		byID := make(map[domain.ContentHash]domain.Snapshot, len(view.Snapshots))
		for _, snapshot := range view.Snapshots {
			byID[snapshot.ID] = snapshot
		}
		for index := range view.Pending {
			if snapshot, ok := byID[view.Pending[index].Target]; ok && !snapshot.CreatedAt.IsZero() {
				view.Pending[index].UpdatedAt = snapshot.CreatedAt
			}
		}
		return domain.ProjectPendingState(ctx, view)
	})
}

var _ inbound.PendingStateQuery = (*Service)(nil)
