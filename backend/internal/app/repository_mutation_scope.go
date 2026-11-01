package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (commands *repositoryRefCommands) ListHistory(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	if commands.configurationError != nil {
		return nil, commands.configurationError
	}
	if commands.history == nil {
		return nil, fmt.Errorf("context history storage unavailable")
	}
	return commands.history.ListHistory(ctx, repo)
}

func (commands *repositoryRefCommands) notifyRefUpdate(ctx context.Context, repo domain.ContentHash, ref domain.Ref, forced, created bool) error {
	if commands.notifications == nil {
		return fmt.Errorf("durable notification storage unavailable")
	}
	return commands.notifications.NotifyRefUpdate(ctx, repo, ref, forced, created)
}

func (commands *repositoryRefCommands) UpdateRef(ctx context.Context, input inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	if commands == nil {
		return inbound.UpdateRefOutput{}, domain.ErrValidation
	}
	if commands.configurationError != nil {
		return inbound.UpdateRefOutput{}, commands.configurationError
	}
	output, err := commands.scope.WithinRefWrite(ctx, input.RepoID, func(bound context.Context) (inbound.UpdateRefOutput, error) {
		return commands.updateRefWithPending(bound, input)
	})
	if err != nil && commands.profile == RepositoryRefComplete {
		return inbound.UpdateRefOutput{}, err
	}
	return output, err
}

func (recorder *historyRecorder) notifyRefUpdate(ctx context.Context, repo domain.ContentHash, ref domain.Ref, forced, created bool) error {
	if recorder.followups == nil {
		return domain.ErrCatalogUnsupported
	}
	return recorder.followups.NotifyRefUpdate(ctx, repo, ref, forced, created)
}

func (recorder *historyRecorder) verifyStoredSnapshotDoc(ctx context.Context, repo domain.ContentHash, snapshot domain.Snapshot) error {
	if recorder.sources == nil {
		return domain.ErrCatalogUnsupported
	}
	return recorder.sources.VerifySnapshotDocument(ctx, repo, snapshot)
}

func (recorder *historyRecorder) queueHistoryGitScan(ctx context.Context, event domain.HistoryEvent) error {
	if recorder.followups == nil {
		return domain.ErrCatalogUnsupported
	}
	return recorder.followups.QueueHistoryGitScan(ctx, event)
}
