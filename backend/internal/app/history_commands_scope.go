package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type historyGitScanQueue interface {
	EnqueueGitScan(context.Context, domain.GitScanJob) error
}

type historyPRWakeQueue interface {
	WakePRSourceJobs(context.Context, domain.ContentHash, time.Time) error
}

type historyCommandScope struct {
	work          *repositoryWork
	transactions  outbound.RepositoryTransactions
	state         outbound.RepositoryTransactionState
	metadata      RepositoryAuthorityMetadata
	repositories  repositoryRefRepositoryReader
	notifications RepositoryNotificationQueue
	gitScans      historyGitScanQueue
	prWake        historyPRWakeQueue
}

func (service *Service) HistoryCommands() (inbound.HistoryCommands, error) {
	if service == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	return historyCommandsFromStores(service.meta, service.blobs, service.engine, service.repositories, service.repositoryWork(), HistoryComplete)
}

func historyCommandsForWork(metadata outbound.MetadataStore, blobs outbound.BlobStore, engine outbound.GitEngine, repositories outbound.RepositoryStore, work *repositoryWork, profile HistoryProfile) *historyRecorder {
	commands, err := historyCommandsFromStores(metadata, blobs, engine, repositories, work, profile)
	if err != nil {
		return &historyRecorder{configurationError: err}
	}
	return commands
}

func historyCommandsFromStores(metadata outbound.MetadataStore, blobs outbound.BlobStore, engine outbound.GitEngine, repositories outbound.RepositoryStore, work *repositoryWork, profile HistoryProfile) (*historyRecorder, error) {
	if work == nil {
		return nil, domain.ErrValidation
	}
	if work.configurationError != nil {
		return nil, work.configurationError
	}
	if profile == HistoryComplete {
		if err := work.validateCompleteAuthority(); err != nil {
			return nil, err
		}
	}
	events, _ := metadata.(HistoryEventStore)
	state, _ := metadata.(outbound.RepositoryTransactionState)
	proofs, _ := metadata.(outbound.HistoryDocumentProofStore)
	stored, _ := blobs.(outbound.StoredDocVerifier)
	legacyNotifications, _ := metadata.(outbound.NotificationStore)
	legacyScans, _ := metadata.(outbound.GitScanStore)
	legacyJobs, _ := metadata.(outbound.PRJobStore)
	var notifications RepositoryNotificationQueue = legacyNotifications
	var scans historyGitScanQueue = legacyScans
	var wakes historyPRWakeQueue = legacyJobs
	for _, dependency := range []any{metadata, blobs, engine, repositories, events, state, proofs, stored, notifications, scans, wakes} {
		if dependency != nil && !rootAdapterPresent(dependency) {
			return nil, domain.ErrValidation
		}
	}
	scope := &historyCommandScope{work: work, transactions: work.transactions, state: state, metadata: metadata,
		repositories: repositories, notifications: notifications, gitScans: scans, prWake: wakes}
	var preparation outbound.HistoryDocumentProofStore
	if proofs != nil && work.transactions != nil && state != nil && events != nil && stored != nil {
		preparation = proofs
	}
	return NewHistoryCommands(HistoryCommandsConfig{Profile: profile, Metadata: metadata, Events: events, Memories: blobs,
		Sources: documentPublicationSourcesFromStores(metadata, blobs, engine, work.supported), Scope: scope,
		Followups: scope, Preparation: preparation, SupportedIdentities: work.supported})
}

func (scope *historyCommandScope) WithinHistoryWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	if scope == nil || scope.work == nil || write == nil {
		return domain.ErrValidation
	}
	return repositoryWriteErrorWithWork(ctx, scope.work, repo, write)
}

func (scope *historyCommandScope) WithinHistoryObservation(ctx context.Context, repo domain.ContentHash, observe func(context.Context) error) error {
	if scope == nil || scope.work == nil || scope.transactions == nil || observe == nil {
		return domain.ErrConflict
	}
	return scope.transactions.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := scope.work.authorizeRepositoryWrite(bound, repo); err != nil {
			return err
		}
		if err := scope.work.checkDocumentIdentity(bound, repo, true); err != nil {
			return err
		}
		return observe(bound)
	})
}

func (scope *historyCommandScope) WithinHistoryRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if scope == nil || scope.work == nil || scope.transactions == nil || scope.state == nil || read == nil {
		return domain.ErrConflict
	}
	return scope.transactions.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if !scope.state.InReadOnlyTransaction(bound) {
			return domain.ErrConflict
		}
		if err := scope.work.checkDocumentIdentity(bound, repo, true); err != nil {
			return err
		}
		return read(bound)
	})
}

func (scope *historyCommandScope) NotifyRefUpdate(ctx context.Context, repo domain.ContentHash, ref domain.Ref, forced, created bool) error {
	return notifyRefUpdateWithSources(ctx, scope.metadata, scope.repositories, scope.notifications, repo, ref, forced, created)
}

func (scope *historyCommandScope) QueueHistoryGitScan(ctx context.Context, event domain.HistoryEvent) error {
	return queueHistoryGitScanWithSources(ctx, scope.metadata, scope.gitScans, event)
}

func (scope *historyCommandScope) WakePRSourceJobs(ctx context.Context, repo domain.ContentHash, now time.Time) error {
	if scope.prWake == nil {
		return nil
	}
	return scope.prWake.WakePRSourceJobs(ctx, repo, now)
}
