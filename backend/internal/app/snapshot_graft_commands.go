package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type SnapshotGraftWriter interface {
	GraftSnapshotParents(context.Context, domain.ContentHash, domain.ContentHash, []domain.ContentHash, uint64) error
}

type SnapshotGraftStore interface {
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	AddGraftParentsCAS(context.Context, domain.ContentHash, domain.ContentHash, []domain.ContentHash, uint64) error
}

type SnapshotGraftWriteScope interface {
	WithinSnapshotGraftWrite(context.Context, domain.ContentHash, func(context.Context) error) error
}

type SnapshotGraftCommandsConfig struct {
	Store SnapshotGraftStore
	Scope SnapshotGraftWriteScope
}

type SnapshotGraftCommands struct {
	store SnapshotGraftStore
	scope SnapshotGraftWriteScope
}

var _ SnapshotGraftWriter = (*SnapshotGraftCommands)(nil)

func NewSnapshotGraftCommands(config SnapshotGraftCommandsConfig) (*SnapshotGraftCommands, error) {
	if !rootAdapterPresent(config.Store) || !rootAdapterPresent(config.Scope) {
		return nil, domain.ErrValidation
	}
	return &SnapshotGraftCommands{store: config.Store, scope: config.Scope}, nil
}

func (commands *SnapshotGraftCommands) GraftSnapshotParents(ctx context.Context, repoID, id domain.ContentHash, parents []domain.ContentHash, expectedSeq uint64) error {
	return commands.scope.WithinSnapshotGraftWrite(ctx, repoID, func(bound context.Context) error {
		return commands.graftSnapshotParents(bound, repoID, id, parents, expectedSeq)
	})
}

func (commands *SnapshotGraftCommands) graftSnapshotParents(ctx context.Context, repoID, id domain.ContentHash, parents []domain.ContentHash, expectedSeq uint64) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(id); err != nil {
		return err
	}
	if len(parents) == 0 || len(parents) > 16 {
		return fmt.Errorf("%w: graft parents must be 1..16", domain.ErrValidation)
	}
	if _, err := commands.store.GetSnapshot(ctx, repoID, id); err != nil {
		return err
	}
	for _, parent := range parents {
		if err := domain.ValidateContentHash(parent); err != nil {
			return err
		}
		if parent == id {
			return fmt.Errorf("%w: self graft", domain.ErrValidation)
		}
		if _, err := commands.store.GetSnapshot(ctx, repoID, parent); err != nil {
			return fmt.Errorf("%w: graft parent %s not found", domain.ErrNotFound, parent)
		}
	}
	return commands.store.AddGraftParentsCAS(ctx, repoID, id, parents, expectedSeq)
}
