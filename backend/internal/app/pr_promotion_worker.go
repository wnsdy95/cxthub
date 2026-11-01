package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type PRPromotionProfile string

const (
	PRPromotionCompatibility PRPromotionProfile = "compatibility"
	PRPromotionComplete      PRPromotionProfile = "complete"
)

type PRPromotionWorkerConfig struct {
	Profile               PRPromotionProfile
	Metadata              outbound.MetadataStore
	Blobs                 outbound.BlobStore
	Engine                outbound.GitEngine
	Repositories          outbound.RepositoryStore
	Evidence              outbound.RepositoryReader
	RequireCompleteMemory bool
	CapturedEvidence      bool
}

type PRPromotionWorker struct {
	meta                  outbound.MetadataStore
	blobs                 outbound.BlobStore
	engine                outbound.GitEngine
	repositories          outbound.RepositoryStore
	work                  *repositoryWork
	refs                  *repositoryRefCommands
	history               *historyRecorder
	evidence              outbound.RepositoryReader
	requireCompleteMemory bool
	capturedEvidence      bool
}

func NewPRPromotionWorker(config PRPromotionWorkerConfig) (*PRPromotionWorker, error) {
	return newPRPromotionWorkerWithWork(config, nil)
}

func newPRPromotionWorkerWithWork(config PRPromotionWorkerConfig, work *repositoryWork) (*PRPromotionWorker, error) {
	if config.Profile != PRPromotionCompatibility && config.Profile != PRPromotionComplete {
		return nil, domain.ErrValidation
	}
	if !rootAdapterPresent(config.Metadata) {
		return nil, domain.ErrValidation
	}
	for _, optional := range []any{config.Blobs, config.Engine, config.Repositories, config.Evidence} {
		if optional != nil && !rootAdapterPresent(optional) {
			return nil, domain.ErrValidation
		}
	}
	jobs, _ := config.Metadata.(outbound.PRJobStore)
	if !rootAdapterPresent(jobs) {
		return nil, domain.ErrCatalogUnsupported
	}
	if config.Profile == PRPromotionComplete {
		if err := validatePRPromotionDependencies(config); err != nil {
			return nil, err
		}
	}
	if work == nil {
		work = newRepositoryWork(config.Metadata, config.Repositories, ReadDocumentIdentities(config.Metadata, config.Blobs))
	}
	if config.Profile == PRPromotionComplete {
		if _, err := repositoryRefsFromStores(config.Metadata, config.Engine, config.Repositories, work, RepositoryRefComplete); err != nil {
			return nil, err
		}
		if _, err := historyCommandsFromStores(config.Metadata, config.Blobs, config.Engine, config.Repositories, work, HistoryComplete); err != nil {
			return nil, err
		}
	}
	return assemblePRPromotionWorker(config, work), nil
}

func validatePRPromotionDependencies(config PRPromotionWorkerConfig) error {
	transactions, _ := config.Metadata.(outbound.RepositoryTransactions)
	snapshots, _ := config.Metadata.(documentQuerySnapshots)
	presence, _ := config.Metadata.(outbound.RepositoryTransactionPresence)
	state, _ := config.Metadata.(outbound.RepositoryTransactionState)
	fence, _ := config.Metadata.(outbound.PRJobFence)
	history, _ := config.Metadata.(outbound.HistoryStore)
	appendStore, _ := config.Metadata.(interface {
		AppendRef(context.Context, domain.ContentHash, domain.Ref, domain.ContentHash) error
	})
	verifier, _ := config.Blobs.(outbound.StoredDocVerifier)
	access, _ := config.Repositories.(outbound.RepositoryAccessLocker)
	reader := config.Evidence
	if reader == nil {
		reader = config.Metadata
	}
	evidence, _ := reader.(outbound.GraphEvidenceReader)
	revisions, _ := config.Metadata.(outbound.RepositoryRevisions)
	notifications, _ := config.Metadata.(outbound.NotificationStore)
	scans, _ := config.Metadata.(outbound.GitScanStore)
	for _, dependency := range []any{config.Blobs, config.Engine, config.Repositories, transactions, snapshots, presence, state, fence, history, appendStore, verifier, access, evidence, revisions, notifications, scans} {
		if !rootAdapterPresent(dependency) {
			return domain.ErrCatalogUnsupported
		}
	}
	return nil
}

func assemblePRPromotionWorker(config PRPromotionWorkerConfig, work *repositoryWork) *PRPromotionWorker {
	reader := config.Evidence
	if reader == nil {
		reader = config.Metadata
	}
	profile := RepositoryRefCompatibility
	if config.Profile == PRPromotionComplete {
		profile = RepositoryRefComplete
	}
	historyProfile := HistoryCompatibility
	if config.Profile == PRPromotionComplete {
		historyProfile = HistoryComplete
	}
	return &PRPromotionWorker{meta: config.Metadata, blobs: config.Blobs, engine: config.Engine,
		repositories: config.Repositories, work: work,
		refs:     repositoryRefCommandsForWork(config.Metadata, config.Engine, config.Repositories, work, profile),
		history:  historyCommandsForWork(config.Metadata, config.Blobs, config.Engine, config.Repositories, work, historyProfile),
		evidence: reader, requireCompleteMemory: config.RequireCompleteMemory, capturedEvidence: config.CapturedEvidence}
}

func (worker *PRPromotionWorker) prJobs() (outbound.PRJobStore, error) {
	return prJobsFromMetadata(worker.meta)
}

func prJobsFromMetadata(metadata outbound.MetadataStore) (outbound.PRJobStore, error) {
	jobs, supported := metadata.(outbound.PRJobStore)
	if !supported {
		return nil, fmt.Errorf("durable PR queue unavailable")
	}
	return jobs, nil
}

func (worker *PRPromotionWorker) Run(ctx context.Context) {
	worker.RunPRPromotionWorker(ctx)
}

func (worker *PRPromotionWorker) workerDocumentIdentityContext(ctx context.Context) context.Context {
	return (gitChangeScope{work: worker.work}).WorkerContext(ctx)
}

func (worker *PRPromotionWorker) ListHistory(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	return listHistoryWithWork(ctx, worker.work, repo)
}

func (worker *PRPromotionWorker) UpdateRef(ctx context.Context, input inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	return worker.refs.UpdateRef(ctx, input)
}

func (worker *PRPromotionWorker) recordHistory(ctx context.Context, event domain.HistoryEvent, receipt bool, verified historyVerification) error {
	return worker.history.recordHistory(ctx, event, receipt, verified)
}

func (worker *PRPromotionWorker) newCodeEvidence(ctx context.Context, repo domain.ContentHash) (*codeEvidence, error) {
	return newCodeEvidenceFromReader(ctx, worker.evidence, repo, worker.requireCompleteMemory)
}

func (worker *PRPromotionWorker) verifiedPRContributions(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]verifiedPRContribution, error) {
	queries, err := newEvidenceQueries(worker.evidence, worker.capturedEvidence)
	if err != nil {
		return nil, err
	}
	return queries.VerifiedPR(ctx, repo, history)
}

func (worker *PRPromotionWorker) contributionDestination(ctx context.Context, repo domain.ContentHash, name, identity string, history []domain.HistoryEvent) (domain.Ref, error) {
	return contributionDestinationFromReader(ctx, worker.meta, repo, name, identity, history)
}

func (worker *PRPromotionWorker) promoteBoundPR(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge, base string) (inbound.UpdateRefOutput, error) {
	return repositoryWriteWithWork(ctx, worker.work, repo, func(bound context.Context) (inbound.UpdateRefOutput, error) {
		return worker.promoteBoundPRInTransaction(bound, repo, request, base)
	})
}
