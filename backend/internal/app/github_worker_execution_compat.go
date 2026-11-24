package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (g *GitHubConnections) reconcile(ctx context.Context, ns string) error {
	return g.worker().reconcile(ctx, ns)
}

func (g *GitHubConnections) deliver(ctx context.Context, id string) error {
	return g.worker().deliver(ctx, id)
}

func (g *GitHubConnections) applyDelivery(ctx context.Context, j domain.GitHubDelivery) error {
	return g.worker().applyDelivery(ctx, j)
}
