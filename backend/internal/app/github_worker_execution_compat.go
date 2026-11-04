package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (g *GitHubConnections) Run(ctx context.Context) {
	g.worker().Run(ctx)
}

func (g *GitHubConnections) Tick(ctx context.Context) error {
	return g.worker().Tick(ctx)
}

func (g *GitHubConnections) reconcile(ctx context.Context, ns string) error {
	return g.worker().reconcile(ctx, ns)
}

func (g *GitHubConnections) binding(ctx context.Context, c domain.GitHubConnection, b domain.GitHubBinding) (domain.Repo, error) {
	return g.worker().binding(ctx, c, b)
}

func (g *GitHubConnections) enqueuePR(ctx context.Context, c domain.GitHubConnection, b domain.GitHubBinding, pr domain.PullRequestMerge) error {
	return g.worker().enqueuePR(ctx, c, b, pr)
}

func (g *GitHubConnections) deliver(ctx context.Context, id string) error {
	return g.worker().deliver(ctx, id)
}

func (g *GitHubConnections) applyDelivery(ctx context.Context, j domain.GitHubDelivery) error {
	return g.worker().applyDelivery(ctx, j)
}
