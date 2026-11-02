package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (connections *GitHubConnections) reconcilePRHistories(ctx context.Context, limit int) error {
	return connections.worker().reconcilePRHistories(ctx, limit)
}

func (connections *GitHubConnections) reconcilePRHistory(ctx context.Context, reader outbound.GitHubPRHistory, storage outbound.PRHistoryStore, item githubPRHistoryWork) error {
	return connections.worker().reconcilePRHistory(ctx, reader, storage, item)
}

func (connections *GitHubConnections) preparePRParticipations(ctx context.Context, storage outbound.PRHistoryStore, checkpoint *domain.PRHistoryCheckpoint, fresh []domain.GitRefObservation) (preparedPRParticipations, error) {
	return connections.worker().preparePRParticipations(ctx, storage, checkpoint, fresh)
}

func (connections *GitHubConnections) checkConnectionWorkerPolicy(ctx context.Context, connection domain.GitHubConnection) error {
	return connections.worker().checkConnectionWorkerPolicy(ctx, connection)
}

func (connections *GitHubConnections) checkDeliveryWorkerPolicy(ctx context.Context, delivery domain.GitHubDelivery) error {
	return connections.worker().checkDeliveryWorkerPolicy(ctx, delivery)
}
