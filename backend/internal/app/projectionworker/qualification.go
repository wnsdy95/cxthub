package projectionworker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync/atomic"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Qualification struct {
	scope        QualificationScope
	jobs         outbound.GraphQualificationStore
	projections  outbound.GraphProjectionStore
	repairs      outbound.GraphProjectionRepairStore
	generations  outbound.RepositoryGraphSourceGenerationStore
	reference    ReferenceReader
	repositories []domain.ContentHash
	next         *atomic.Uint64
}

func NewQualification(config QualificationConfig) (*Qualification, error) {
	if !validProfile(config.Profile) {
		return nil, fmt.Errorf("%w: qualification profile is required", domain.ErrValidation)
	}
	if !present(config.Scope) {
		return nil, fmt.Errorf("%w: qualification scope is required", domain.ErrValidation)
	}
	if config.Profile == Complete {
		for _, dependency := range []any{config.Jobs, config.Projections, config.Repairs, config.Generations, config.Reference} {
			if dependency != nil && !present(dependency) {
				return nil, fmt.Errorf("%w: typed-nil qualification dependency", domain.ErrValidation)
			}
		}
		if len(config.Repositories) > 0 && (config.Jobs == nil || config.Projections == nil || config.Repairs == nil || config.Generations == nil || config.Reference == nil) {
			return nil, domain.ErrCatalogUnsupported
		}
	}
	for _, repo := range config.Repositories {
		if err := domain.ValidateContentHash(repo); err != nil {
			return nil, err
		}
	}
	next := config.Next
	if next == nil {
		next = &atomic.Uint64{}
	}
	return &Qualification{scope: config.Scope, jobs: config.Jobs, projections: config.Projections,
		repairs: config.Repairs, generations: config.Generations, reference: config.Reference,
		repositories: slices.Clone(config.Repositories), next: next}, nil
}

func (qualification *Qualification) Process(ctx context.Context, limit int) error {
	if limit <= 0 || limit > 128 {
		return domain.ErrValidation
	}
	if len(qualification.repositories) == 0 {
		return nil
	}
	if !present(qualification.jobs) {
		return domain.ErrCatalogUnsupported
	}
	if err := qualification.scope.ValidateQualificationStart(ctx); err != nil {
		return err
	}
	ctx = qualification.scope.WorkerContext(ctx)
	processed := 0
	for range qualification.repositories {
		index := (qualification.next.Add(1) - 1) % uint64(len(qualification.repositories))
		repo := qualification.repositories[index]
		claimed := false
		err := qualification.scope.WithinWork(ctx, func(bound context.Context) error {
			lease, err := func() (domain.GraphQualificationLease, error) {
				claim, cancel := context.WithTimeout(bound, 5*time.Second)
				defer cancel()
				return qualification.jobs.ClaimGraphQualification(claim, repo, time.Minute)
			}()
			if err != nil {
				return err
			}
			if lease.RepoID != repo {
				return domain.ErrIntegrity
			}
			claimed = true
			return qualification.RunLease(bound, lease)
		})
		if !claimed && errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		processed++
		if processed >= limit {
			break
		}
	}
	return nil
}

func (qualification *Qualification) RunLease(ctx context.Context, lease domain.GraphQualificationLease) error {
	if err := lease.Validate(); err != nil {
		return err
	}
	if !present(qualification.jobs) {
		return domain.ErrCatalogUnsupported
	}
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result domain.GraphQualificationResult
	err := qualification.scope.WithinRead(work, func(bound context.Context) error {
		var compareErr error
		result, compareErr = qualification.Compare(bound, lease)
		return compareErr
	})
	if err == nil {
		finish, release := context.WithTimeout(ctx, 5*time.Second)
		defer release()
		return qualification.jobs.FinishGraphQualification(finish, lease, result)
	}
	if ctx.Err() != nil {
		return err
	}
	reason := "read_error"
	switch {
	case QualificationCorruption(lease, err) != nil:
	case errors.Is(err, domain.ErrGraphProjectionNotReady), errors.Is(err, domain.ErrCatalogResetRequired):
		reason = "not_current"
	case errors.Is(err, domain.ErrRepositoryViewSourceLimit), errors.Is(err, domain.ErrResourceOverloaded):
		reason = "resource_limit"
	}
	finish, release := context.WithTimeout(ctx, 5*time.Second)
	defer release()
	failureErr := qualification.jobs.FailGraphQualification(finish, lease, reason)
	return errors.Join(err, failureErr, qualification.Repair(finish, lease, err))
}

func (qualification *Qualification) Compare(ctx context.Context, lease domain.GraphQualificationLease) (domain.GraphQualificationResult, error) {
	var result domain.GraphQualificationResult
	if !qualification.scope.InReadOnlyTransaction(ctx) {
		return result, domain.ErrConflict
	}
	if !present(qualification.generations) {
		return result, domain.ErrCatalogUnsupported
	}
	if !present(qualification.projections) {
		return result, domain.ErrCatalogUnsupported
	}
	checkpoint, err := qualification.generations.ReadRepositoryGraphSourceGeneration(ctx, lease.RepoID)
	if err != nil {
		return result, err
	}
	if checkpoint.Validate() != nil {
		return result, domain.ErrIntegrity
	}
	identity := checkpoint
	identity.Sequence = lease.Applied.Sequence
	if identity != lease.Applied || checkpoint.Sequence < lease.Applied.Sequence || lease.ReferenceExpected == "" && checkpoint != lease.Applied {
		return result, domain.ErrGraphProjectionNotReady
	}
	projection, err := qualification.projections.ReadGraphProjection(ctx, lease.RepoID)
	if err != nil {
		return result, err
	}
	if _, err := projection.Status(); err != nil {
		return result, err
	}
	if projection.Source != checkpoint {
		return result, domain.ErrIntegrity
	}
	if !projection.Enabled || projection.NeedsAttention || !projection.Complete || lease.ReferenceExpected == "" && !projection.Ready || projection.QualificationKey() != lease.GraphQualificationKey {
		return result, domain.ErrGraphProjectionNotReady
	}
	result.Expected = lease.ReferenceExpected
	if result.Expected == "" {
		if !present(qualification.reference) {
			return result, domain.ErrCatalogUnsupported
		}
		result.Expected, err = qualification.reference.Reference(ctx, lease.RepoID)
	} else {
		err = qualification.scope.CheckIdentity(ctx, lease.RepoID)
	}
	if err != nil {
		return result, err
	}
	_, err = domain.DecodeGraphViewProjection(ctx, lease.RepoID, projection.Parts)
	if err != nil {
		failure := &domain.GraphProjectionCorruption{RepoID: lease.RepoID, Dataset: projection.Dataset,
			Applied: projection.Applied, Rules: projection.Rules, Cause: err}
		if failure.Validate() == nil {
			return result, failure
		}
		return result, err
	}
	result.GraphQualificationKey = lease.GraphQualificationKey
	result.Actual, err = domain.GraphViewProjectionManifestDigest(ctx, lease.RepoID, projection.Parts)
	return result, err
}

func (qualification *Qualification) Repair(ctx context.Context, lease domain.GraphQualificationLease, readErr error) error {
	failure := QualificationCorruption(lease, readErr)
	if failure == nil {
		return nil
	}
	if !present(qualification.repairs) {
		return domain.ErrCatalogUnsupported
	}
	return qualification.repairs.RequestGraphProjectionRepair(ctx, *failure)
}

func QualificationCorruption(lease domain.GraphQualificationLease, readErr error) *domain.GraphProjectionCorruption {
	var failure *domain.GraphProjectionCorruption
	if !errors.As(readErr, &failure) || failure == nil || failure.Serving || failure.Validate() != nil ||
		failure.RepoID != lease.RepoID || failure.Dataset != lease.Dataset || failure.Applied != lease.Applied || failure.Rules != lease.Rules {
		return nil
	}
	return failure
}

func (qualification *Qualification) Run(ctx context.Context) {
	if len(qualification.repositories) == 0 {
		return
	}
	for {
		if err := qualification.Process(ctx, 1); err != nil && ctx.Err() == nil {
			log.Printf("graph qualification: %v", err)
		}
		timer := time.NewTimer(time.Minute)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
