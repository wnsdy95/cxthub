package app

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const memoryReadAnchorRetention = 6 * time.Hour
const memoryReadResultRetention = 30 * time.Minute

var errMemoryReadCleanupPending = errors.New("expired memory preparation cleanup is progressing")

func (service *Service) ConfigureMemoryReadPreparations(ctx context.Context, repos []domain.ContentHash) error {
	if len(repos) > domain.MaxMemoryReadWorkerRepos {
		return domain.ErrValidation
	}
	if len(repos) == 0 {
		service.memoryReadRepos = nil
		return nil
	}
	storage, supported := service.meta.(outbound.MemoryReadWorkStore)
	_, captures := service.meta.(outbound.MemoryCaptureStore)
	_, anchors := service.meta.(outbound.MemoryCaptureAnchorStore)
	_, results := service.meta.(outbound.MemoryReadStore)
	_, current := service.meta.(outbound.CurrentMemoryReadStore)
	_, reconcile := service.meta.(outbound.MemoryReadWorkReconciler)
	_, dispatch := service.meta.(outbound.MemoryReadObjectWakeDispatcher)
	_, minimum := service.meta.(outbound.MinimumMemoryReadWorkStore)
	_, comparison := service.meta.(outbound.MemoryReadComparisonStore)
	if !supported || !captures || !anchors || !results || !current || !reconcile || !dispatch || !minimum || !comparison {
		return domain.ErrCatalogUnsupported
	}
	for _, repo := range repos {
		if err := domain.ValidateContentHash(repo); err != nil {
			return err
		}
	}
	configured := make(map[domain.ContentHash]bool, len(repos))
	var selected []domain.ContentHash
	for _, repo := range repos {
		if configured[repo] {
			continue
		}
		if err := storage.ConfigureMemoryReadWork(ctx, repo, true); err != nil {
			return err
		}
		configured[repo] = true
		selected = append(selected, repo)
	}
	service.memoryReadRepos = selected
	return nil
}

func (workers *MemoryPreparationWorkers) ProcessMemoryReadPreparations(ctx context.Context, limit int) error {
	return workers.processMemoryReadPreparations(ctx, limit, nil)
}

func (workers *MemoryPreparationWorkers) processMemoryReadPreparations(ctx context.Context, limit int, anchor **PreparedMemoryRead) error {
	if limit <= 0 || limit > 128 {
		return domain.ErrValidation
	}
	if len(workers.memoryReadRepos) == 0 {
		return nil
	}
	storage, supported := workers.meta.(outbound.MemoryReadWorkStore)
	if !supported {
		return nil
	}
	reconciler, supported := workers.meta.(outbound.MemoryReadWorkReconciler)
	if !supported {
		return domain.ErrCatalogUnsupported
	}
	dispatcher, supported := workers.meta.(outbound.MemoryReadObjectWakeDispatcher)
	if !supported {
		return domain.ErrCatalogUnsupported
	}
	ctx = workers.DocumentIdentityWorkerContext(inbound.WithSystemActor(ctx))
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), workers.DocumentIdentitiesSupported())
	for processed := 0; processed < limit; processed++ {
		claimed := false
		err := admittedJob(ctx, func(bound context.Context) (domain.MemoryReadWork, error) {
			claim, cancel := context.WithTimeout(bound, 5*time.Second)
			defer cancel()
			if _, err := dispatcher.DispatchMemoryReadObjectWakes(claim, 1); err != nil {
				return domain.MemoryReadWork{}, err
			}
			if _, err := reconciler.ReconcileMemoryReadWork(claim, workers.memoryReadRepos, 1); err != nil {
				return domain.MemoryReadWork{}, err
			}
			return storage.ClaimMemoryReadWork(claim, workers.memoryReadRepos, time.Minute)
		}, func(bound context.Context, job domain.MemoryReadWork) error {
			claimed = true
			return workers.runMemoryReadPreparation(bound, storage, job, anchor)
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

func (workers *MemoryPreparationWorkers) runMemoryReadPreparation(ctx context.Context, storage outbound.MemoryReadWorkStore, job domain.MemoryReadWork, anchor **PreparedMemoryRead) error {
	work, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := workers.prepareMemoryReadWork(work, job, anchor)
	if err == nil || ctx.Err() != nil {
		return err
	}
	reason, retry := memoryReadWorkFailure(err)
	finish, release := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer release()
	failure := storage.FailMemoryReadWork(finish, job, reason, retry)
	if errors.Is(err, errMemoryReadCleanupPending) {
		return failure
	}
	return errors.Join(err, failure)
}

func (workers *MemoryPreparationWorkers) prepareMemoryReadWork(ctx context.Context, job domain.MemoryReadWork, anchor **PreparedMemoryRead) error {
	if err := job.Validate(); err != nil {
		return err
	}
	if job.State != "running" {
		return domain.ErrConflict
	}
	_, err := repositoryReadForRepoWithWork(ctx, workers.work, job.Target.RepoID, func(context.Context) (struct{}, error) { return struct{}{}, nil })
	if err != nil {
		return err
	}
	captures, supported := workers.meta.(outbound.MemoryCaptureStore)
	if !supported {
		return domain.ErrCatalogUnsupported
	}
	removed, err := captures.PruneMemoryCaptures(ctx, job.Target.RepoID, 32)
	if err != nil {
		return err
	}
	if removed == 32 {
		return errMemoryReadCleanupPending
	}
	var previous *PreparedMemoryRead
	if anchor != nil {
		previous = *anchor
	}
	expiresAt := time.Now().UTC().Add(memoryReadResultRetention)
	request := domain.EffectiveMemoryRequest{Selection: job.Target.Selection, Content: job.Target.Content}
	prepared, err := workers.prepareIncrementalMemoryRead(ctx, job.Target.RepoID, request, expiresAt, previous, true)
	if err != nil {
		return err
	}
	if err := domain.CheckMemorySourceMinimum(prepared.qualification.Source, job.Source); err != nil {
		return err
	}
	if prepared.input.Header.Version == domain.MemoryCaptureVersion {
		expiresAt = time.Now().UTC().Add(memoryReadAnchorRetention)
	}
	if err := workers.persistPreparedMemoryRead(ctx, prepared, expiresAt); err != nil {
		return err
	}
	comparisons, supported := workers.meta.(outbound.MemoryReadComparisonStore)
	if !supported {
		return domain.ErrCatalogUnsupported
	}
	if prepared.comparison == nil {
		return domain.ErrIntegrity
	}
	if err := comparisons.RecordMemoryReadComparison(ctx, job, *prepared.comparison); err != nil {
		return err
	}
	if !prepared.comparison.Matched() {
		return domain.ErrMemoryReadComparisonMismatch
	}
	minimum, supported := workers.meta.(outbound.MinimumMemoryReadWorkStore)
	if !supported {
		return domain.ErrCatalogUnsupported
	}
	if _, err := minimum.FinishMinimumMemoryReadWork(ctx, job, prepared.qualification); err != nil {
		return err
	}
	if anchor != nil {
		*anchor = &PreparedMemoryRead{graphAnchor: prepared.graphAnchor}
	}
	return nil
}

func memoryReadWorkFailure(err error) (string, bool) {
	switch {
	case errors.Is(err, errMemoryReadCleanupPending):
		return "retention_cleanup", true
	case errors.Is(err, domain.ErrMemoryReadComparisonMismatch):
		return "comparison_mismatch", false
	case errors.Is(err, domain.ErrRefConflict), errors.Is(err, domain.ErrConflict):
		return "source_changed", true
	case errors.Is(err, domain.ErrCatalogResetRequired):
		return "source_reset", true
	case errors.Is(err, domain.ErrMemoryCaptureExpired):
		return "retention_expired", true
	case errors.Is(err, domain.ErrRepositoryViewSourceLimit), errors.Is(err, domain.ErrMemoryProjectionLimit):
		return "resource_limit", false
	case errors.Is(err, domain.ErrIntegrity), errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrCatalogUnsupported):
		return "invalid_source", false
	case errors.Is(err, domain.ErrForbidden):
		return "access_denied", false
	case errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired):
		return "identity_upgrade_required", true
	default:
		return "temporary_failure", true
	}
}

func (workers *MemoryPreparationWorkers) RunMemoryReadPreparationWorker(ctx context.Context) {
	if len(workers.memoryReadRepos) == 0 {
		return
	}
	var anchor *PreparedMemoryRead
	for {
		if err := workers.processMemoryReadPreparations(ctx, 16, &anchor); err != nil && ctx.Err() == nil {
			log.Printf("memory read preparation: %v", err)
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
