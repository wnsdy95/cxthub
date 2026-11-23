package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type historyCompatibilityReadKey struct{}

var historyCompatibilityStop = errors.New("review capture stop; never mutate history")

type historyCompatibilityHistoryBoundary struct {
	*store.FSStore
	captureCalls         int
	captureCompatibility error
}

func (p *historyCompatibilityHistoryBoundary) WithinRepository(ctx context.Context, _ domain.ContentHash, fn func(context.Context) error) error {
	return fn(ctx)
}
func (p *historyCompatibilityHistoryBoundary) WithinReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	return fn(context.WithValue(ctx, historyCompatibilityReadKey{}, true))
}
func (p *historyCompatibilityHistoryBoundary) InReadOnlyTransaction(ctx context.Context) bool {
	return ctx.Value(historyCompatibilityReadKey{}) == true
}
func (p *historyCompatibilityHistoryBoundary) Capture(ctx context.Context, snaps []domain.Snapshot) error {
	p.captureCalls++
	if len(snaps) != 1 || snaps[0].DocIdentity != domain.DocumentIdentityRootV1 {
		return domain.ErrIntegrity
	}
	p.captureCompatibility = outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityRootV1)
	if p.captureCompatibility != nil {
		return p.captureCompatibility
	}
	return historyCompatibilityStop
}
func (p *historyCompatibilityHistoryBoundary) Pin(context.Context) error {
	panic("review capture stop must prevent pin")
}
