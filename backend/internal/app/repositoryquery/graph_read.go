package repositoryquery

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/app/graphdiagnostics"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *PublishedGraphQueries) GetRepositoryView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	return readPublishedGraphResult(ctx, queries, repo, func(bound context.Context) (domain.RepositoryView, error) {
		return queries.readPublishedGraphView(bound, repo)
	})
}

func (queries *PublishedGraphQueries) GetPendingView(ctx context.Context, repo domain.ContentHash) (domain.PendingView, error) {
	return readPublishedGraphResult(ctx, queries, repo, func(bound context.Context) (domain.PendingView, error) {
		view, err := queries.readPublishedGraphView(bound, repo)
		if err != nil {
			return domain.PendingView{}, err
		}
		return domain.ProjectPendingView(view)
	})
}

func (queries *PublishedGraphQueries) QueryGraphState(ctx context.Context, repo domain.ContentHash, position string) (domain.GraphState, error) {
	return readPublishedGraphResult(ctx, queries, repo, func(bound context.Context) (domain.GraphState, error) {
		return queries.readPublishedGraph(bound, repo, position)
	})
}

func readPublishedGraphResult[Result any](ctx context.Context, queries *PublishedGraphQueries, repo domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	graphdiagnostics.SelectRoute(ctx, graphdiagnostics.Published)
	ctx, span := graphdiagnostics.Start(ctx, graphdiagnostics.PublishedRead)
	defer span.End()
	var result Result
	if err := domain.ValidateContentHash(repo); err != nil {
		return result, err
	}
	if !queries.repos[repo] {
		return result, domain.ErrCatalogUnsupported
	}
	err := queries.scope.ReadGraph(ctx, repo, func(bound context.Context) error {
		_, admission := graphdiagnostics.Start(bound, graphdiagnostics.BuildAdmission)
		bound, release, err := queries.resources.AcquireBuild(bound)
		admission.End()
		if err != nil {
			return err
		}
		defer release()
		if err := bound.Err(); err != nil {
			return err
		}
		result, err = read(bound)
		return err
	})
	if err != nil {
		var empty Result
		return empty, queries.repairGraphRead(ctx, repo, err)
	}
	return result, nil
}

func (queries *PublishedGraphQueries) readPublishedGraphView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	projection, err := graphdiagnostics.Read(ctx, graphdiagnostics.PublishedLoad, func(bound context.Context) (domain.GraphProjectionRead, error) {
		return queries.reader.ReadQualifiedGraphProjection(bound, repo)
	})
	if err != nil {
		return domain.RepositoryView{}, err
	}
	if projection.Source.RepoID != repo || projection.Complete && projection.Applied.RepoID != repo {
		return domain.RepositoryView{}, domain.ErrIntegrity
	}
	status, err := projection.Status()
	if err != nil {
		return domain.RepositoryView{}, err
	}
	if projection.NeedsAttention {
		return domain.RepositoryView{}, domain.ErrGraphProjectionFailed
	}
	if !projection.Enabled || !projection.Complete {
		return domain.RepositoryView{}, domain.ErrGraphProjectionNotReady
	}
	view, err := graphdiagnostics.Read(ctx, graphdiagnostics.PublishedDecode, func(bound context.Context) (domain.RepositoryView, error) {
		return domain.DecodeGraphViewProjection(bound, repo, projection.Parts)
	})
	if err != nil {
		failure := &domain.GraphProjectionCorruption{RepoID: repo, Dataset: projection.Dataset,
			Applied: projection.Applied, Rules: projection.Rules, Serving: true, ServingFence: projection.ServingFence, Cause: err}
		if failure.Validate() == nil {
			return domain.RepositoryView{}, failure
		}
		return domain.RepositoryView{}, err
	}
	_, qualification := graphdiagnostics.Start(ctx, graphdiagnostics.PublishedQualification)
	err = domain.ValidateGraphReadQualification(ctx, projection)
	qualification.End()
	if err != nil {
		return domain.RepositoryView{}, err
	}
	generation := domain.GraphReadGeneration{
		Generation: status.Generation, SourceEpoch: status.SourceEpoch, AppliedEpoch: status.AppliedEpoch,
		Rules: status.Rules, Complete: true, Current: projection.Ready,
	}
	view.ReadGeneration = &generation
	graphGeneration := generation
	view.Graph.ReadGeneration = &graphGeneration
	return view, nil
}

func (queries *PublishedGraphQueries) readPublishedGraph(ctx context.Context, repo domain.ContentHash, position string) (domain.GraphState, error) {
	view, err := queries.readPublishedGraphView(ctx, repo)
	if err != nil {
		return domain.GraphState{}, err
	}
	if position == "" {
		return *view.Graph, nil
	}
	if err := ctx.Err(); err != nil {
		return domain.GraphState{}, err
	}
	_, positionSpan := graphdiagnostics.Start(ctx, graphdiagnostics.PositionProjection)
	graph, err := ProjectGraphPosition(view, position)
	positionSpan.End()
	if err != nil {
		if !view.ReadGeneration.Current && errors.Is(err, domain.ErrNotFound) {
			err = domain.ErrGraphProjectionNotReady
		}
		return domain.GraphState{}, err
	}
	graph.ReadGeneration = view.ReadGeneration
	return graph, ctx.Err()
}

func (queries *PublishedGraphQueries) repairGraphRead(ctx context.Context, repo domain.ContentHash, readErr error) error {
	var failure *domain.GraphProjectionCorruption
	if !queries.repos[repo] || !errors.As(readErr, &failure) || failure == nil ||
		failure.RepoID != repo || failure.Validate() != nil || !queries.scope.GraphRepairAllowed(ctx) {
		return readErr
	}
	ctx, repair := graphdiagnostics.Start(ctx, graphdiagnostics.Repair)
	defer repair.End()
	err := queries.scope.RepairGraph(ctx, repo, func(bound context.Context) error {
		if failure.Serving {
			return queries.servingRepairs.RequestGraphServingRepair(bound, *failure)
		}
		return queries.projectionRepairs.RequestGraphProjectionRepair(bound, *failure)
	})
	if err != nil {
		return errors.Join(readErr, fmt.Errorf("schedule graph projection repair: %w", err))
	}
	return readErr
}
