package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type prPublicationCommands struct {
	work                  *repositoryWork
	evidence              outbound.RepositoryReader
	memory                outbound.MemoryObjectReader
	requireCompleteMemory bool
	revisions             inbound.RepositoryStateQueries
	revisionError         error
}

func (service *Service) prPublications() *prPublicationCommands {
	return service.prPublicationsWithWork(service.repositoryWork())
}

func (service *Service) prPublicationsWithWork(work *repositoryWork) *prPublicationCommands {
	revisions, revisionError := service.RepositoryStateQueries()
	return &prPublicationCommands{work: work, evidence: service.graphEvidenceReader(),
		memory: service.memoryProjectionReader(), requireCompleteMemory: service.requireCompleteMemory,
		revisions: revisions, revisionError: revisionError}
}

func (commands *prPublicationCommands) SubmitPRPromotion(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge) (domain.PRPromotionJob, error) {
	if commands == nil || commands.work == nil {
		return domain.PRPromotionJob{}, domain.ErrValidation
	}
	return commands.submitPRPromotion(ctx, repo, request)
}

func (commands *prPublicationCommands) prJobs() (outbound.PRJobStore, error) {
	return prJobsFromMetadata(commands.work.meta)
}

func (commands *prPublicationCommands) ListHistory(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	return listHistoryWithWork(ctx, commands.work, repo)
}

func (commands *prPublicationCommands) queuePRGitScans(ctx context.Context, repo domain.ContentHash, origin string, request domain.PullRequestMerge) error {
	return queuePRGitScansFromStore(ctx, commands.work.meta, repo, origin, request)
}

func (commands *prPublicationCommands) contributionDestination(ctx context.Context, repo domain.ContentHash, name, identity string, history []domain.HistoryEvent) (domain.Ref, error) {
	return contributionDestinationFromReader(ctx, commands.work.meta, repo, name, identity, history)
}

func (commands *prPublicationCommands) newCodeEvidence(ctx context.Context, repo domain.ContentHash) (*codeEvidence, error) {
	return newCodeEvidenceFromReader(ctx, commands.evidence, repo, commands.requireCompleteMemory)
}

func (commands *prPublicationCommands) verifyContributionClosure(ctx context.Context, contribution domain.PRContribution) error {
	return verifyContributionCaptureFromReader(ctx, commands.work.meta, contribution.RepoID, contribution.Capture)
}

func (commands *prPublicationCommands) contributionPublicationMemorySource(ctx context.Context, repo, capture domain.ContentHash, publication domain.HistoryEvent) (domain.ContentHash, error) {
	return contributionPublicationMemorySourceFromReader(ctx, commands.memory, repo, capture, publication)
}

func (commands *prPublicationCommands) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	if commands.revisionError != nil {
		return domain.RepositoryRevision{}, commands.revisionError
	}
	return commands.revisions.RepositoryRevision(ctx, repo)
}
