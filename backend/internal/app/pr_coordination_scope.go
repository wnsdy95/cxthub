package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type prCoordinationScope struct {
	work *repositoryWork
}

func (service *Service) PRPromotionCommands() (inbound.PRPromotionCommands, error) {
	if service == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	return service.prCoordinationFromService(PRCoordinationComplete, service.repositoryWork())
}

func (service *Service) prCoordinations() *prCoordinationCommands {
	commands, err := service.prCoordinationFromService(PRCoordinationCompatibility, service.repositoryWork())
	if err != nil {
		return &prCoordinationCommands{configurationError: err}
	}
	return commands
}

func (service *Service) prCoordinationFromService(profile PRCoordinationProfile, work *repositoryWork) (*prCoordinationCommands, error) {
	if work == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	if work.configurationError != nil {
		return nil, work.configurationError
	}
	config := service.PRPromotionWorkerConfig(PRPromotionCompatibility)
	if profile == PRCoordinationComplete {
		if err := work.validateCompleteAuthority(); err != nil {
			return nil, err
		}
		if work.transactions != nil {
			config.Profile = PRPromotionComplete
		}
	}
	var executor *PRPromotionWorker
	if profile == PRCoordinationComplete {
		var err error
		executor, err = newPRPromotionWorkerWithWork(config, work)
		if err != nil {
			return nil, err
		}
	} else {
		executor = assemblePRPromotionWorker(config, work)
	}
	jobs, _ := service.meta.(outbound.PRJobStore)
	associations, _ := service.meta.(outbound.PRJobAssociationStore)
	return NewPRPromotionCommands(PRPromotionCommandsConfig{Profile: profile, Scope: &prCoordinationScope{work: work},
		Jobs: jobs, Submission: service.prPublicationsWithWork(work), Executor: executor, Associations: associations,
		SupportedIdentities: work.supported})
}

func (scope *prCoordinationScope) WithinPRSubmit(ctx context.Context, repo domain.ContentHash, submit func(context.Context) (domain.PRPromotionJob, error)) (domain.PRPromotionJob, error) {
	if scope == nil || scope.work == nil || submit == nil {
		return domain.PRPromotionJob{}, domain.ErrValidation
	}
	ctx = auditOperation(ctx, "context.promotion.submitted")
	ctx = context.WithValue(ctx, revisionScopeKey{}, "none")
	return repositoryWriteWithWork(ctx, scope.work, repo, submit)
}

func (scope *prCoordinationScope) WithinPRRetry(ctx context.Context, repo domain.ContentHash, retry func(context.Context) error) error {
	if scope == nil || scope.work == nil || retry == nil {
		return domain.ErrValidation
	}
	return repositoryWriteErrorWithWork(context.WithValue(ctx, revisionScopeKey{}, "none"), scope.work, repo, retry)
}

func (scope *prCoordinationScope) WithinPRRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) ([]domain.PRPromotionJob, error)) ([]domain.PRPromotionJob, error) {
	if scope == nil || scope.work == nil || read == nil {
		return nil, domain.ErrValidation
	}
	return repositoryReadForRepoWithWork(ctx, scope.work, repo, read)
}

func (worker *PRPromotionWorker) PromoteBoundPR(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge, baseID string) (inbound.UpdateRefOutput, error) {
	if worker == nil {
		return inbound.UpdateRefOutput{}, domain.ErrValidation
	}
	return worker.promoteBoundPR(ctx, repo, request, baseID)
}

func (worker *PRPromotionWorker) RunPRJob(ctx context.Context, job domain.PRPromotionJob) (inbound.UpdateRefOutput, error) {
	if worker == nil {
		return inbound.UpdateRefOutput{}, domain.ErrValidation
	}
	return worker.runPRJob(ctx, job)
}
