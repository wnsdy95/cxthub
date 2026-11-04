package repositoryquery

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *Queries) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	return readRepository(ctx, queries, repo, func(bound context.Context) (domain.RepositoryRevision, error) {
		if err := domain.ValidateContentHash(repo); err != nil {
			return domain.RepositoryRevision{}, err
		}
		if queries.revisions != nil {
			return queries.revisions.RepositoryRevision(bound, repo)
		}
		return domain.RepositoryRevision{}, nil
	})
}

func (queries *Queries) RepositoryChangeState(ctx context.Context, repo domain.ContentHash) (domain.RepositoryChangeState, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.RepositoryChangeState{}, err
	}
	return readRepository(ctx, queries, repo, func(bound context.Context) (domain.RepositoryChangeState, error) {
		revision, err := queries.RepositoryRevision(bound, repo)
		if err != nil {
			return domain.RepositoryChangeState{}, err
		}
		state := domain.RepositoryChangeState{RepositoryRevision: revision}
		if queries.pendingRepos[repo] {
			if queries.pendingState == nil {
				return domain.RepositoryChangeState{}, domain.ErrCatalogUnsupported
			}
			projection, err := queries.pendingState.ReadPendingProjectionStatus(bound, repo)
			if err != nil {
				return domain.RepositoryChangeState{}, err
			}
			status, err := projection.Status()
			if err != nil {
				return domain.RepositoryChangeState{}, err
			}
			state.PendingProjection = &status
		}
		if queries.graphRepos[repo] {
			if queries.graphState == nil {
				return domain.RepositoryChangeState{}, domain.ErrCatalogUnsupported
			}
			projection, err := queries.graphState.ReadQualifiedGraphProjectionStatus(bound, repo)
			if err != nil {
				return domain.RepositoryChangeState{}, err
			}
			if projection.Source.RepoID != repo {
				return domain.RepositoryChangeState{}, domain.ErrIntegrity
			}
			status, err := projection.Status()
			if err != nil {
				return domain.RepositoryChangeState{}, err
			}
			if projection.Enabled && !projection.NeedsAttention && projection.Complete {
				if err := domain.ValidateGraphReadQualification(bound, projection); err != nil {
					switch {
					case errors.Is(err, domain.ErrGraphProjectionNotReady):
						status.State = "catching_up"
					case errors.Is(err, domain.ErrGraphProjectionFailed):
						status.State = "needs_attention"
					default:
						return domain.RepositoryChangeState{}, err
					}
					status.Complete, status.Current, status.Generation = false, false, ""
				}
			}
			state.GraphProjection = &status
		}
		return state, nil
	})
}
