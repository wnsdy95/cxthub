package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type repositoryRefRepositoryReader interface {
	GetRepository(context.Context, string) (domain.Repository, error)
}

type repositoryRefHistoryReader interface {
	ListHistoryEvents(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error)
}

type repositoryRefScope struct {
	work          *repositoryWork
	metadata      RepositoryAuthorityMetadata
	repositories  repositoryRefRepositoryReader
	history       repositoryRefHistoryReader
	notifications RepositoryNotificationQueue
}

func (service *Service) RepositoryRefCommands() (inbound.RepositoryRefCommands, error) {
	if service == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	return repositoryRefsFromStores(service.meta, service.engine, service.repositories, service.repositoryWork(), RepositoryRefComplete)
}

func repositoryRefCommandsForWork(metadata outbound.MetadataStore, engine outbound.GitEngine, repositories outbound.RepositoryStore, work *repositoryWork, profile RepositoryRefProfile) *repositoryRefCommands {
	commands, err := repositoryRefsFromStores(metadata, engine, repositories, work, profile)
	if err != nil {
		return &repositoryRefCommands{configurationError: err}
	}
	return commands
}

func repositoryRefsFromStores(metadata outbound.MetadataStore, engine outbound.GitEngine, repositories outbound.RepositoryStore, work *repositoryWork, profile RepositoryRefProfile) (*repositoryRefCommands, error) {
	if work == nil {
		return nil, domain.ErrValidation
	}
	if work.configurationError != nil {
		return nil, work.configurationError
	}
	if profile == RepositoryRefComplete {
		if err := work.validateCompleteAuthority(); err != nil {
			return nil, err
		}
	}
	history, _ := metadata.(outbound.HistoryStore)
	appendStore, _ := metadata.(RepositoryRefAppend)
	revisions, _ := metadata.(RepositoryRefRevisions)
	var notifications RepositoryNotificationQueue
	if profile == RepositoryRefComplete {
		notifications, _ = metadata.(RepositoryNotificationQueue)
	} else {
		legacy, _ := metadata.(outbound.NotificationStore)
		notifications = legacy
	}
	for _, dependency := range []any{history, notifications} {
		if dependency != nil && !rootAdapterPresent(dependency) {
			return nil, domain.ErrValidation
		}
	}
	if profile == RepositoryRefComplete && notifications == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	scope := &repositoryRefScope{work: work, metadata: metadata, repositories: repositories, history: history, notifications: notifications}
	var historyPort RepositoryRefHistory
	if history != nil {
		historyPort = scope
	}
	return NewRepositoryRefCommands(RepositoryRefCommandsConfig{Profile: profile, Store: metadata, Engine: engine,
		Scope: scope, History: historyPort, Append: appendStore, Revisions: revisions, Notifications: scope,
		Transactional: work.transactions != nil})
}

func (scope *repositoryRefScope) WithinRefWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (inbound.UpdateRefOutput, error)) (inbound.UpdateRefOutput, error) {
	if scope == nil || scope.work == nil || write == nil {
		return inbound.UpdateRefOutput{}, domain.ErrValidation
	}
	return repositoryWriteWithWork(auditOperation(ctx, "context.ref.updated"), scope.work, repo, write)
}

func (scope *repositoryRefScope) WithinRefsWrite(ctx context.Context, repo domain.ContentHash, empty bool, write func(context.Context) (inbound.UpdateRefsOutput, error)) (inbound.UpdateRefsOutput, error) {
	if scope == nil || scope.work == nil || write == nil {
		return inbound.UpdateRefsOutput{}, domain.ErrValidation
	}
	ctx = auditOperation(ctx, "context.refs.updated")
	if empty {
		ctx = pendingWriteContext(ctx)
	}
	return repositoryWriteWithWork(ctx, scope.work, repo, write)
}

func (scope *repositoryRefScope) ListHistory(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	if scope == nil || scope.work == nil || scope.history == nil {
		return nil, fmt.Errorf("context history storage unavailable")
	}
	return repositoryReadForRepoWithWork(ctx, scope.work, repo, func(bound context.Context) ([]domain.HistoryEvent, error) {
		return scope.history.ListHistoryEvents(bound, repo)
	})
}

func (scope *repositoryRefScope) NotifyRefUpdate(ctx context.Context, repo domain.ContentHash, ref domain.Ref, forced, created bool) error {
	if scope == nil {
		return domain.ErrValidation
	}
	return notifyRefUpdateWithSources(ctx, scope.metadata, scope.repositories, scope.notifications, repo, ref, forced, created)
}
