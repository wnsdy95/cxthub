package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type contextBranchHTTPProbe struct {
	calls      []string
	calledRepo domain.ContentHash
	input      any
	err        error
	observe    func(context.Context)
	history    []domain.HistoryEvent
}

func (owner *contextBranchHTTPProbe) record(ctx context.Context, operation string, repo domain.ContentHash, input any) {
	owner.calls, owner.calledRepo, owner.input = append(owner.calls, operation), repo, input
	if owner.observe != nil {
		owner.observe(ctx)
	}
}
func (owner *contextBranchHTTPProbe) EnableContextProtocol(ctx context.Context, repo domain.ContentHash) error {
	owner.record(ctx, "protocol", repo, nil)
	return owner.err
}
func (owner *contextBranchHTTPProbe) ListHistory(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	owner.record(ctx, "history", repo, nil)
	return owner.history, owner.err
}
