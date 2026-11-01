package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type branchHistoryIndex = contextprojection.HistoryIndex
type branchQueryInputs = contextprojection.Inputs

func newBranchHistoryIndex(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) (*branchHistoryIndex, error) {
	return contextprojection.NewHistoryIndex(ctx, repo, history)
}

func newBranchQueryInputs(ctx context.Context, repo domain.ContentHash, snapshots []domain.Snapshot, history []domain.HistoryEvent) (*branchQueryInputs, error) {
	return contextprojection.NewInputs(ctx, repo, snapshots, history)
}
