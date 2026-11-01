package projectionworker

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Graph struct {
	scope       GraphScope
	jobs        outbound.GraphProjectionStore
	source      outbound.RepositoryGraphSourceStore
	factory     outbound.GraphSourceReaderFactory
	references  outbound.GraphProjectionReferenceStore
	generations outbound.RepositoryGraphSourceGenerationStore
	reference   ReferenceReader
	enabled     bool
}

func NewGraph(config GraphConfig) (*Graph, error) {
	if !validProfile(config.Profile) {
		return nil, domain.ErrValidation
	}
	if !present(config.Scope) {
		return nil, domain.ErrCatalogUnsupported
	}
	if config.Profile == Complete {
		for _, required := range []any{config.Jobs, config.Source, config.Factory} {
			if !present(required) {
				return nil, domain.ErrCatalogUnsupported
			}
		}
		for _, optional := range []any{config.References, config.Generations, config.Reference} {
			if optional != nil && !present(optional) {
				return nil, domain.ErrCatalogUnsupported
			}
		}
		if present(config.References) && (!present(config.Generations) || !present(config.Reference)) {
			return nil, domain.ErrCatalogUnsupported
		}
	}
	return &Graph{scope: config.Scope, jobs: config.Jobs, source: config.Source, factory: config.Factory,
		references: config.References, generations: config.Generations, reference: config.Reference, enabled: config.Enabled}, nil
}

func (graph *Graph) Process(ctx context.Context, limit int) error {
	if limit <= 0 || limit > 128 {
		return domain.ErrValidation
	}
	if !present(graph.jobs) {
		return nil
	}
	if !present(graph.source) || !present(graph.factory) {
		return domain.ErrCatalogUnsupported
	}
	ctx = graph.scope.WorkerContext(inbound.WithSystemActor(ctx))
	for processed := 0; processed < limit; processed++ {
		claimed := false
		err := graph.scope.WithinWork(ctx, func(bound context.Context) error {
			job, err := func() (domain.GraphProjectionLease, error) {
				claim, cancel := context.WithTimeout(bound, 30*time.Second)
				defer cancel()
				return graph.jobs.ClaimGraphProjection(claim, time.Minute)
			}()
			if err != nil {
				return err
			}
			claimed = true
			return graph.RunLease(bound, job)
		})
		if !claimed && errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (graph *Graph) RunLease(ctx context.Context, job domain.GraphProjectionLease) error {
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := graph.scope.CheckRepository(work, job.RepoID)
	if err == nil {
		switch job.Phase {
		case "capture":
			if present(graph.references) && job.Cursor == "" {
				var page domain.RepositoryViewSourcePage
				var reference domain.GraphProjectionReference
				page, reference, err = CaptureFirstPage(work, graph.scope, graph.source, graph.generations, graph.reference, job)
				if err == nil {
					err = graph.references.SaveGraphProjectionPageWithReference(work, job, page, reference)
				}
				break
			}
			request := job.SourceRequest()
			request.Limit = domain.DefaultRepositoryViewSourceLimit
			var page domain.RepositoryViewSourcePage
			page, err = graph.source.RepositoryGraphSourceChanges(work, job.RepoID, request)
			if err == nil {
				err = ValidateGraphProjectionPage(work, job, page)
			}
			if err == nil {
				err = graph.jobs.SaveGraphProjectionPage(work, job, page)
			}
		case "project":
			err = ProjectGraphProjection(work, graph.jobs, graph.factory, job, graph.scope)
		default:
			err = domain.ErrIntegrity
		}
	}
	if err == nil || ctx.Err() != nil {
		return err
	}
	reason, retry := "temporary_failure", true
	switch {
	case errors.Is(err, domain.ErrCatalogResetRequired):
		reason = "source_reset"
	case errors.Is(err, domain.ErrRefConflict), errors.Is(err, domain.ErrConflict):
		reason = "lease_lost"
	case errors.Is(err, domain.ErrRepositoryViewSourceLimit):
		reason, retry = "resource_limit", false
	case errors.Is(err, domain.ErrIntegrity), errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrNotFound):
		reason, retry = "invalid_source", false
	case errors.Is(err, domain.ErrForbidden):
		reason, retry = "access_denied", false
	case errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired):
		reason = "identity_upgrade_required"
	}
	finish, release := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer release()
	return errors.Join(err, graph.jobs.FailGraphProjection(finish, job, reason, retry))
}

func ValidateGraphProjectionPage(ctx context.Context, job domain.GraphProjectionLease, page domain.RepositoryViewSourcePage) error {
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: page.Version, Scope: page.Scope, RepoID: page.RepoID, Epoch: page.Epoch, Sequence: page.Through}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	if checkpoint.Scope != domain.RepositoryGraphSourceScope || checkpoint.Epoch != job.Epoch {
		return domain.ErrCatalogResetRequired
	}
	mode := "baseline"
	if job.AppliedSequence >= 0 {
		mode = "delta"
	}
	if checkpoint.RepoID != job.RepoID || checkpoint.Sequence < job.AppliedSequence || page.Mode != mode {
		return domain.ErrIntegrity
	}
	if len(page.Entries) > domain.DefaultRepositoryViewSourceLimit || len(page.NextCursor) > 16<<10 {
		return domain.ErrRepositoryViewSourceLimit
	}
	if page.NextCursor == "" {
		if page.Checkpoint == nil || *page.Checkpoint != checkpoint {
			return domain.ErrIntegrity
		}
	} else if page.Checkpoint != nil || len(page.Entries) == 0 || page.NextCursor == job.Cursor {
		return domain.ErrIntegrity
	}
	size := 0
	for _, entry := range page.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		size += len(entry.Key) + len(entry.Value) + 128
		if size > domain.MaxRepositoryViewSourcePageBytes || len(entry.Key) > domain.MaxRepositoryViewSourceKeyBytes || len(entry.Value) > domain.MaxRepositoryViewSourceImageBytes {
			return domain.ErrRepositoryViewSourceLimit
		}
	}
	return ctx.Err()
}

func CaptureFirstPage(ctx context.Context, scope GraphScope, source outbound.RepositoryGraphSourceStore, generation outbound.RepositoryGraphSourceGenerationStore, reference ReferenceReader, job domain.GraphProjectionLease) (domain.RepositoryViewSourcePage, domain.GraphProjectionReference, error) {
	if job.Cursor != "" || job.Phase != "capture" {
		return domain.RepositoryViewSourcePage{}, domain.GraphProjectionReference{}, domain.ErrValidation
	}
	if !present(scope) || !present(source) {
		return domain.RepositoryViewSourcePage{}, domain.GraphProjectionReference{}, domain.ErrCatalogUnsupported
	}
	var page domain.RepositoryViewSourcePage
	var captured domain.GraphProjectionReference
	err := scope.WithinRepositoryRead(ctx, job.RepoID, func(bound context.Context) error {
		request := job.SourceRequest()
		request.Limit = domain.DefaultRepositoryViewSourceLimit
		var err error
		page, err = source.RepositoryGraphSourceChanges(bound, job.RepoID, request)
		if err != nil {
			return err
		}
		if err := ValidateGraphProjectionPage(bound, job, page); err != nil {
			return err
		}
		if !present(generation) {
			return domain.ErrCatalogUnsupported
		}
		checkpoint, err := generation.ReadRepositoryGraphSourceGeneration(bound, job.RepoID)
		if err != nil {
			return err
		}
		if checkpoint != (domain.RepositoryViewSourceCheckpoint{Version: page.Version, Scope: page.Scope, RepoID: page.RepoID, Epoch: page.Epoch, Sequence: page.Through}) {
			return domain.ErrConflict
		}
		if !present(reference) {
			return domain.ErrCatalogUnsupported
		}
		expected, err := reference.Reference(bound, job.RepoID)
		if err != nil {
			return err
		}
		captured = domain.GraphProjectionReference{GraphQualificationKey: domain.GraphQualificationKey{
			RepoID: job.RepoID, Dataset: job.Dataset, Applied: checkpoint,
			Rules: domain.GraphViewProjectionRules, ComparisonRules: domain.GraphQualificationRules}, Expected: expected}
		if err := captured.Validate(); err != nil {
			return err
		}
		return bound.Err()
	})
	if err != nil {
		return domain.RepositoryViewSourcePage{}, domain.GraphProjectionReference{}, err
	}
	return page, captured, nil
}

func ProjectGraphProjection(ctx context.Context, jobs outbound.GraphProjectionStore, factory outbound.GraphSourceReaderFactory, job domain.GraphProjectionLease, resources repositoryquery.GraphResources) error {
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope, RepoID: job.RepoID, Epoch: job.Epoch, Sequence: job.InputSequence}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	if job.Cursor != "" || job.AppliedSequence < -1 || job.InputSequence <= job.AppliedSequence {
		return domain.ErrIntegrity
	}
	entries, err := jobs.LoadGraphProjectionInputs(ctx, job)
	if err != nil {
		return err
	}
	if len(entries) > domain.MaxPendingProjectionInputRows {
		return domain.ErrRepositoryViewSourceLimit
	}
	size := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		size += graphSourceEntryBytes(entry)
		if size > domain.MaxPendingProjectionInputBytes || len(entry.Key) > domain.MaxRepositoryViewSourceKeyBytes || len(entry.Value) > domain.MaxRepositoryViewSourceImageBytes {
			return domain.ErrRepositoryViewSourceLimit
		}
		if entry.Deleted || entry.Sequence < 0 || entry.Sequence > checkpoint.Sequence || !domain.RepositoryGraphSourceKindValid(entry.Kind) {
			return domain.ErrIntegrity
		}
	}
	metadata, view, proof, err := repositoryquery.DecodeCapturedGraphInputs(ctx, checkpoint, entries)
	if err != nil {
		return err
	}
	evidence, err := factory.BuildGraphSourceReader(ctx, checkpoint, metadata, proof)
	if err != nil {
		return err
	}
	view, err = repositoryquery.ProjectCapturedRepositoryView(ctx, checkpoint, view, evidence, resources)
	if err != nil {
		return err
	}
	parts, err := domain.EncodeGraphViewProjection(ctx, job.RepoID, view)
	if err != nil {
		return err
	}
	return jobs.PublishGraphProjection(ctx, job, parts)
}

func graphSourceEntryBytes(entry domain.RepositoryViewSourceEntry) int {
	return len(entry.Kind) + len(entry.Key) + len(entry.Value) + 128
}

func (graph *Graph) Run(ctx context.Context) {
	if !graph.enabled {
		return
	}
	for {
		if err := graph.Process(ctx, 16); err != nil && ctx.Err() == nil {
			log.Printf("graph projection: %v", err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
