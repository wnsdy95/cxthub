package repositoryquery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *Queries) GetPendingState(ctx context.Context, repo domain.ContentHash) (domain.PendingStateView, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.PendingStateView{}, err
	}
	ctx, release, err := queries.resources.AcquireInteractive(ctx)
	if err != nil {
		return domain.PendingStateView{}, err
	}
	defer release()
	ctx, err = queries.scope.AuthorizeRead(ctx, repo, queries.pendingRepos[repo])
	if err != nil {
		return domain.PendingStateView{}, err
	}
	return readRepository(ctx, queries, repo, func(bound context.Context) (domain.PendingStateView, error) {
		if queries.pendingRepos[repo] {
			if queries.pending == nil {
				return domain.PendingStateView{}, domain.ErrCatalogUnsupported
			}
			projection, err := queries.pending.ReadPendingProjection(bound, repo)
			if err != nil {
				return domain.PendingStateView{}, err
			}
			if projection.Enabled {
				if projection.NeedsAttention {
					return domain.PendingStateView{}, domain.ErrPendingProjectionFailed
				}
				if !projection.Ready || projection.Source != projection.Applied {
					return domain.PendingStateView{}, domain.ErrPendingProjectionNotReady
				}
				view, err := domain.DecodePendingProjection(bound, repo, projection.Parts)
				if err != nil {
					return domain.PendingStateView{}, err
				}
				status, err := projection.Status()
				if err != nil {
					return domain.PendingStateView{}, err
				}
				view.ReadGeneration = &domain.PendingReadGeneration{Generation: status.Generation, SourceEpoch: status.SourceEpoch, AppliedEpoch: status.SourceEpoch, Rules: status.Rules, Complete: true}
				return view, nil
			}
		}
		view, err := LoadRepositoryView(bound, repo, RepositoryViewReaders{
			Metadata: queries.metadata, History: queries.history, Revisions: queries.revisions, Archives: queries.archives,
		})
		if err != nil {
			return domain.PendingStateView{}, err
		}
		return domain.ProjectPendingState(bound, view)
	})
}
