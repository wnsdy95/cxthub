package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (connections *GitHubConnections) checkConnectionWorkerPolicy(ctx context.Context, connection domain.GitHubConnection) error {
	return connections.worker().checkConnectionWorkerPolicy(ctx, connection)
}

func (connections *GitHubConnections) checkDeliveryWorkerPolicy(ctx context.Context, delivery domain.GitHubDelivery) error {
	return connections.worker().checkDeliveryWorkerPolicy(ctx, delivery)
}
