package repositoryquery

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

var _ inbound.GraphReadinessQuery = (*Queries)(nil)

func (queries *Queries) GetGraphReadiness(ctx context.Context, repo domain.ContentHash) (domain.GraphReadiness, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.GraphReadiness{}, err
	}
	if queries.graphState == nil || !queries.atomicRead {
		return domain.GraphReadiness{}, domain.ErrCatalogUnsupported
	}
	ctx, release, err := queries.resources.AcquireInteractive(ctx)
	if err != nil {
		return domain.GraphReadiness{}, err
	}
	defer release()
	ctx, err = queries.scope.AuthorizeRead(ctx, repo, true)
	if err != nil {
		return domain.GraphReadiness{}, err
	}
	return readRepository(ctx, queries, repo, func(bound context.Context) (domain.GraphReadiness, error) {
		status, err := queries.graphReadiness(bound, repo)
		if err != nil {
			return domain.GraphReadiness{}, err
		}
		return domain.GraphReadiness{Selected: queries.graphRepos[repo], Projection: status}, bound.Err()
	})
}

func (queries *Queries) graphReadiness(ctx context.Context, repo domain.ContentHash) (domain.GraphProjectionStatus, error) {
	projection, err := queries.graphState.ReadQualifiedGraphProjectionStatus(ctx, repo)
	if err != nil {
		return domain.GraphProjectionStatus{}, err
	}
	if projection.Source.RepoID != repo || projection.Complete && projection.Applied.RepoID != repo || len(projection.Parts) != 0 {
		return domain.GraphProjectionStatus{}, domain.ErrIntegrity
	}
	status, err := projection.Status()
	if err != nil {
		return domain.GraphProjectionStatus{}, err
	}
	if projection.Enabled && !projection.NeedsAttention && projection.Complete {
		if err := domain.ValidateGraphReadQualification(ctx, projection); err != nil {
			switch {
			case errors.Is(err, domain.ErrGraphProjectionNotReady):
				status.State = "catching_up"
			case errors.Is(err, domain.ErrGraphProjectionFailed):
				status.State = "needs_attention"
			default:
				return domain.GraphProjectionStatus{}, err
			}
			status.Complete, status.Current, status.Generation = false, false, ""
		}
	}
	return status, nil
}
