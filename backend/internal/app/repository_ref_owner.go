package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type RepositoryRefProfile string

const (
	RepositoryRefCompatibility RepositoryRefProfile = "compatibility"
	RepositoryRefComplete      RepositoryRefProfile = "complete"
)

type RepositoryRefStore interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	GetRef(context.Context, domain.ContentHash, domain.RefKind, string) (domain.Ref, error)
	CompareAndSwapRef(context.Context, domain.ContentHash, domain.Ref, domain.ContentHash) error
	ApplyBranchLifecycleRef(context.Context, domain.ContentHash, domain.Ref) error
	ListPendings(context.Context, domain.ContentHash) ([]domain.Pending, error)
	ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error)
	ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error)
	CompareAndDeletePending(context.Context, domain.ContentHash, string, domain.ContentHash) (domain.PendingDeleteResult, error)
}

type RepositoryRefEngine interface {
	ClassifyRefMove(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash) (outbound.RefMoveClass, error)
}

type RepositoryRefWriteScope interface {
	WithinRefWrite(context.Context, domain.ContentHash, func(context.Context) (inbound.UpdateRefOutput, error)) (inbound.UpdateRefOutput, error)
	WithinRefsWrite(context.Context, domain.ContentHash, bool, func(context.Context) (inbound.UpdateRefsOutput, error)) (inbound.UpdateRefsOutput, error)
}

type RepositoryRefHistory interface {
	ListHistory(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error)
}

type RepositoryRefAppend interface {
	AppendRef(context.Context, domain.ContentHash, domain.Ref, domain.ContentHash) error
}

type RepositoryRefRevisions interface {
	RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error)
}

type RepositoryRefNotifications interface {
	NotifyRefUpdate(context.Context, domain.ContentHash, domain.Ref, bool, bool) error
}

type RepositoryRefCommandsConfig struct {
	Profile       RepositoryRefProfile
	Store         RepositoryRefStore
	Engine        RepositoryRefEngine
	Scope         RepositoryRefWriteScope
	History       RepositoryRefHistory
	Append        RepositoryRefAppend
	Revisions     RepositoryRefRevisions
	Notifications RepositoryRefNotifications
	Transactional bool
}

type RepositoryRefCommands = repositoryRefCommands

var _ inbound.RepositoryRefCommands = (*RepositoryRefCommands)(nil)

func NewRepositoryRefCommands(config RepositoryRefCommandsConfig) (*RepositoryRefCommands, error) {
	if config.Profile != RepositoryRefCompatibility && config.Profile != RepositoryRefComplete {
		return nil, domain.ErrValidation
	}
	if !rootAdapterPresent(config.Store) || !rootAdapterPresent(config.Scope) {
		return nil, domain.ErrValidation
	}
	for _, dependency := range []any{config.Engine, config.History, config.Append, config.Revisions, config.Notifications} {
		if dependency != nil && !rootAdapterPresent(dependency) {
			return nil, domain.ErrValidation
		}
	}
	if config.Profile == RepositoryRefComplete {
		for _, dependency := range []any{config.Engine, config.History, config.Append, config.Revisions, config.Notifications} {
			if dependency == nil {
				return nil, domain.ErrCatalogUnsupported
			}
		}
	}
	return &repositoryRefCommands{profile: config.Profile, meta: config.Store, engine: config.Engine,
		scope: config.Scope, history: config.History, append: config.Append, revisions: config.Revisions,
		notifications: config.Notifications, transactional: config.Transactional}, nil
}
