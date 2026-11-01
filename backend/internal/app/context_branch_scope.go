package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type contextBranchScope struct {
	work    *repositoryWork
	roles   repositoryRoleSources
	locker  outbound.RepositoryAccessLocker
	pending *repositoryRefCommands
}

func (service *Service) ContextBranchOperations() (inbound.ContextBranchOperations, error) {
	return service.contextBranches(ContextBranchComplete)
}

func (service *Service) branchOperations() *contextBranchOperations {
	commands, err := service.contextBranches(ContextBranchCompatibility)
	if err != nil {
		return &contextBranchOperations{configurationError: err}
	}
	return commands
}

func (service *Service) contextBranches(profile ContextBranchProfile) (*ContextBranchOperations, error) {
	if service == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	return service.contextBranchesForWork(profile, service.repositoryWork())
}

func (service *Service) contextBranchesForWork(profile ContextBranchProfile, work *repositoryWork) (*ContextBranchOperations, error) {
	if work == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	if work.configurationError != nil {
		return nil, work.configurationError
	}
	if profile == ContextBranchComplete {
		if err := work.validateCompleteAuthority(); err != nil {
			return nil, err
		}
	}
	scope := &contextBranchScope{work: work, pending: repositoryRefCommandsForWork(service.meta, service.engine, service.repositories, work, RepositoryRefCompatibility)}
	if work.authority != nil {
		scope.roles = repositoryRoleSources{repositories: work.authority.repositories, teams: work.authority.teams, organizations: work.authority.organizations}
		scope.locker = work.authority.locker
	}
	history, _ := service.meta.(outbound.HistoryStore)
	protocol, _ := service.meta.(ContextBranchProtocol)
	historyProfile := HistoryCompatibility
	if profile == ContextBranchComplete {
		historyProfile = HistoryComplete
	}
	recorder, err := historyCommandsFromStores(service.meta, service.blobs, service.engine, service.repositories, work, historyProfile)
	if err != nil {
		if profile == ContextBranchComplete {
			return nil, err
		}
		recorder = &historyRecorder{configurationError: err}
	}
	return NewContextBranchOperations(ContextBranchOperationsConfig{Profile: profile, Store: service.meta, Scope: scope,
		History: history, Recorder: recorder, Protocol: protocol, Access: scope, Pending: scope})
}

func (scope *contextBranchScope) WithinFork(ctx context.Context, repo domain.ContentHash, write func(context.Context) (inbound.ForkOutput, error)) (inbound.ForkOutput, error) {
	if scope == nil || scope.work == nil || write == nil {
		return inbound.ForkOutput{}, domain.ErrValidation
	}
	return repositoryWriteWithWork(auditOperation(ctx, "context.forked"), scope.work, repo, write)
}

func (scope *contextBranchScope) WithinJoin(ctx context.Context, repo domain.ContentHash, write func(context.Context) (inbound.JoinOutput, error)) (inbound.JoinOutput, error) {
	if scope == nil || scope.work == nil || write == nil {
		return inbound.JoinOutput{}, domain.ErrValidation
	}
	return repositoryWriteWithWork(ctx, scope.work, repo, write)
}

func (scope *contextBranchScope) WithinProtocol(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	if scope == nil || scope.work == nil || write == nil {
		return domain.ErrValidation
	}
	return repositoryWriteErrorWithWork(writeAction(ctx, "manage"), scope.work, repo, write)
}

func (scope *contextBranchScope) WithinPreview(ctx context.Context, repo domain.ContentHash, read func(context.Context) (inbound.JoinPreviewOutput, error)) (inbound.JoinPreviewOutput, error) {
	if scope == nil || scope.work == nil || read == nil {
		return inbound.JoinPreviewOutput{}, domain.ErrValidation
	}
	return repositoryReadForRepoWithWork(ctx, scope.work, repo, read)
}

func (scope *contextBranchScope) WithinHistory(ctx context.Context, repo domain.ContentHash, read func(context.Context) ([]domain.HistoryEvent, error)) ([]domain.HistoryEvent, error) {
	if scope == nil || scope.work == nil || read == nil {
		return nil, domain.ErrValidation
	}
	return repositoryReadForRepoWithWork(ctx, scope.work, repo, read)
}

func (scope *contextBranchScope) AuthorizeJoin(ctx context.Context, repo domain.ContentHash, actor string, lock bool) error {
	if actor == "" {
		return domain.ErrUnauthorized
	}
	if scope == nil || scope.work == nil || scope.roles.repositories == nil {
		return domain.ErrForbidden
	}
	metadata, err := scope.work.meta.GetRepo(ctx, repo)
	if err != nil {
		return err
	}
	if metadata.RepositoryID == "" {
		return domain.ErrForbidden
	}
	if lock && scope.work.transactions != nil {
		if scope.locker == nil {
			return domain.ErrForbidden
		}
		if err := scope.locker.LockRepositoryAccess(ctx, metadata.RepositoryID, actor); err != nil {
			return err
		}
	}
	record, err := scope.roles.repositories.GetRepository(ctx, metadata.RepositoryID)
	if err != nil {
		return err
	}
	if record.Archived {
		return domain.ErrForbidden
	}
	role, exists, err := scope.roles.roleFor(ctx, record, actor)
	if err != nil {
		return err
	}
	if !exists || !role.AtLeast(domain.RoleMember) {
		return domain.ErrForbidden
	}
	return nil
}

func (scope *contextBranchScope) ReconcileSharedPending(ctx context.Context, repo domain.ContentHash) error {
	if scope == nil || scope.pending == nil {
		return domain.ErrCatalogUnsupported
	}
	return scope.pending.reconcileSharedPendingPointers(ctx, repo)
}
