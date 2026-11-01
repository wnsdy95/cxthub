package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type PRCoordinationProfile string

const (
	PRCoordinationCompatibility PRCoordinationProfile = "compatibility"
	PRCoordinationComplete      PRCoordinationProfile = "complete"
)

type PRPromotionJobControl interface {
	GetPRJob(context.Context, domain.ContentHash, string) (domain.PRPromotionJob, error)
	ListPRJobs(context.Context, domain.ContentHash) ([]domain.PRPromotionJob, error)
	ClaimPRJob(context.Context, domain.ContentHash, string, time.Time, time.Duration) (domain.PRPromotionJob, error)
	RetryPRJob(context.Context, domain.ContentHash, string, time.Time) error
}

type PRPromotionSubmission interface {
	SubmitPRPromotion(context.Context, domain.ContentHash, domain.PullRequestMerge) (domain.PRPromotionJob, error)
}

type PRPromotionExecutor interface {
	PromoteBoundPR(context.Context, domain.ContentHash, domain.PullRequestMerge, string) (inbound.UpdateRefOutput, error)
	RunPRJob(context.Context, domain.PRPromotionJob) (inbound.UpdateRefOutput, error)
}

type PRPromotionScope interface {
	WithinPRSubmit(context.Context, domain.ContentHash, func(context.Context) (domain.PRPromotionJob, error)) (domain.PRPromotionJob, error)
	WithinPRRetry(context.Context, domain.ContentHash, func(context.Context) error) error
	WithinPRRead(context.Context, domain.ContentHash, func(context.Context) ([]domain.PRPromotionJob, error)) ([]domain.PRPromotionJob, error)
}

type PRPromotionCommandsConfig struct {
	Profile             PRCoordinationProfile
	Scope               PRPromotionScope
	Jobs                PRPromotionJobControl
	Submission          PRPromotionSubmission
	Executor            PRPromotionExecutor
	Associations        outbound.PRJobAssociationStore
	SupportedIdentities []domain.DocumentIdentity
}

type prCoordinationCommands struct {
	scope              PRPromotionScope
	jobs               PRPromotionJobControl
	submission         PRPromotionSubmission
	executor           PRPromotionExecutor
	associations       outbound.PRJobAssociationStore
	supported          []domain.DocumentIdentity
	profile            PRCoordinationProfile
	configurationError error
}

type PRPromotionCommands = prCoordinationCommands

var _ inbound.PRPromotionCommands = (*PRPromotionCommands)(nil)

func NewPRPromotionCommands(config PRPromotionCommandsConfig) (*PRPromotionCommands, error) {
	if config.Profile != PRCoordinationCompatibility && config.Profile != PRCoordinationComplete {
		return nil, domain.ErrValidation
	}
	if !rootAdapterPresent(config.Scope) {
		return nil, domain.ErrValidation
	}
	for _, dependency := range []any{config.Jobs, config.Submission, config.Executor, config.Associations} {
		if dependency != nil && !rootAdapterPresent(dependency) {
			return nil, domain.ErrValidation
		}
	}
	if config.Profile == PRCoordinationComplete {
		for _, dependency := range []any{config.Jobs, config.Submission, config.Executor} {
			if dependency == nil {
				return nil, domain.ErrCatalogUnsupported
			}
		}
	}
	return &prCoordinationCommands{scope: config.Scope, jobs: config.Jobs, submission: config.Submission,
		executor: config.Executor, associations: config.Associations,
		supported: append([]domain.DocumentIdentity(nil), config.SupportedIdentities...), profile: config.Profile}, nil
}

func (commands *prCoordinationCommands) ready() error {
	if commands == nil {
		return domain.ErrValidation
	}
	if commands.configurationError != nil {
		return commands.configurationError
	}
	if commands.scope == nil {
		return domain.ErrValidation
	}
	return nil
}

func (commands *prCoordinationCommands) SubmitPRPromotion(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge) (domain.PRPromotionJob, error) {
	if err := commands.ready(); err != nil {
		return domain.PRPromotionJob{}, err
	}
	job, err := commands.scope.WithinPRSubmit(ctx, repo, func(bound context.Context) (domain.PRPromotionJob, error) {
		if commands.submission == nil {
			return domain.PRPromotionJob{}, domain.ErrCatalogUnsupported
		}
		return commands.submission.SubmitPRPromotion(bound, repo, request)
	})
	if err != nil && commands.profile == PRCoordinationComplete {
		return domain.PRPromotionJob{}, err
	}
	return job, err
}

func (commands *prCoordinationCommands) DeliverPRPromotion(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge) (inbound.UpdateRefOutput, error) {
	if err := commands.ready(); err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), commands.supported)
	job, err := commands.SubmitPRPromotion(ctx, repo, request)
	if err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	ctx, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	defer release()
	if commands.executor == nil {
		return inbound.UpdateRefOutput{}, domain.ErrCatalogUnsupported
	}
	var output inbound.UpdateRefOutput
	if job.State == "completed" {
		output, err = commands.executor.PromoteBoundPR(ctx, repo, request, job.BaseBranchID)
	} else {
		if commands.jobs == nil {
			return inbound.UpdateRefOutput{}, domain.ErrCatalogUnsupported
		}
		job, err = commands.jobs.ClaimPRJob(ctx, repo, job.ID, time.Now().UTC(), prJobLease)
		if errors.Is(err, domain.ErrNotFound) {
			return inbound.UpdateRefOutput{}, fmt.Errorf("%w: PR promotion is durably queued", domain.ErrConflict)
		}
		if err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		output, err = commands.executor.RunPRJob(ctx, job)
	}
	if err != nil && commands.profile == PRCoordinationComplete {
		return inbound.UpdateRefOutput{}, err
	}
	return output, err
}

func (commands *prCoordinationCommands) PromoteRepositoryPR(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge) (inbound.UpdateRefOutput, error) {
	if err := commands.ready(); err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	if commands.executor == nil {
		return inbound.UpdateRefOutput{}, domain.ErrCatalogUnsupported
	}
	output, err := commands.executor.PromoteBoundPR(ctx, repo, request, "")
	if err != nil && commands.profile == PRCoordinationComplete {
		return inbound.UpdateRefOutput{}, err
	}
	return output, err
}

func (commands *prCoordinationCommands) RetryPRPromotion(ctx context.Context, repo domain.ContentHash, id string) error {
	if err := commands.ready(); err != nil {
		return err
	}
	return commands.scope.WithinPRRetry(ctx, repo, func(bound context.Context) error {
		if commands.jobs == nil {
			return domain.ErrCatalogUnsupported
		}
		return commands.jobs.RetryPRJob(bound, repo, id, time.Now().UTC())
	})
}

func (commands *prCoordinationCommands) ListPRPromotions(ctx context.Context, repo domain.ContentHash) ([]domain.PRPromotionJob, error) {
	if err := commands.ready(); err != nil {
		return nil, err
	}
	jobs, err := commands.scope.WithinPRRead(ctx, repo, func(bound context.Context) ([]domain.PRPromotionJob, error) {
		if commands.jobs == nil {
			return nil, domain.ErrCatalogUnsupported
		}
		jobs, err := commands.jobs.ListPRJobs(bound, repo)
		if err != nil {
			return nil, err
		}
		return commands.projectPRJobAssociations(bound, repo, jobs)
	})
	if err != nil && commands.profile == PRCoordinationComplete {
		return nil, err
	}
	return jobs, err
}
