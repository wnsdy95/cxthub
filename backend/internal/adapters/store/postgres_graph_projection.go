//go:build postgres

package store

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GraphProjectionStore = (*PostgresStore)(nil)
var _ outbound.GraphProjectionStatusStore = (*PostgresStore)(nil)
var _ outbound.GraphSourceReaderFactory = (*PostgresStore)(nil)

func (s *PostgresStore) graphProjection() projectionStore {
	return projectionStore{PostgresStore: s, spec: projectionSpec{
		scope: domain.RepositoryGraphSourceScope,
		rules: domain.GraphViewProjectionRules,
		work:  "graph_projection_work", inputs: "graph_projection_inputs",
		parts: "graph_projection_parts", ready: "graph_projection_ready",
		sourceSQL: repositoryGraphSourceStateSQL,
		kindValid: domain.RepositoryGraphSourceKindValid,
		decodeParts: func(ctx context.Context, repo domain.ContentHash, parts []domain.PendingProjectionPart) error {
			_, err := domain.DecodeGraphViewProjection(ctx, repo, parts)
			return err
		},
		partKeys:          domain.GraphViewProjectionPartKeys,
		repairQuarantined: graphProjectionRepairQuarantined,
		claimHeldSQL:      graphProjectionClaimHeldSQL,
	}}
}

func (s *PostgresStore) ConfigureGraphProjection(ctx context.Context, repo domain.ContentHash, enabled bool) error {
	return s.graphProjection().configure(ctx, repo, enabled)
}

func (s *PostgresStore) ClaimGraphProjection(ctx context.Context, duration time.Duration) (domain.GraphProjectionLease, error) {
	lease, err := s.graphProjection().claim(ctx, duration)
	return domain.GraphProjectionLease(lease), err
}

func (s *PostgresStore) RenewGraphProjection(ctx context.Context, lease domain.GraphProjectionLease, duration time.Duration) error {
	return s.graphProjection().renew(ctx, domain.PendingProjectionLease(lease), duration)
}

func (s *PostgresStore) SaveGraphProjectionPage(ctx context.Context, lease domain.GraphProjectionLease, page domain.RepositoryViewSourcePage) error {
	return s.graphProjection().savePage(ctx, domain.PendingProjectionLease(lease), page)
}

func (s *PostgresStore) LoadGraphProjectionInputs(ctx context.Context, lease domain.GraphProjectionLease) ([]domain.RepositoryViewSourceEntry, error) {
	return s.graphProjection().loadInputs(ctx, domain.PendingProjectionLease(lease))
}

func (s *PostgresStore) PublishGraphProjection(ctx context.Context, lease domain.GraphProjectionLease, parts []domain.GraphProjectionPart) error {
	return s.graphProjection().publish(ctx, domain.PendingProjectionLease(lease), parts)
}

func (s *PostgresStore) FailGraphProjection(ctx context.Context, lease domain.GraphProjectionLease, reason string, retry bool) error {
	return s.graphProjection().fail(ctx, domain.PendingProjectionLease(lease), reason, retry)
}

func (s *PostgresStore) ReadGraphProjection(ctx context.Context, repo domain.ContentHash) (domain.GraphProjectionRead, error) {
	return s.readGraphProjection(ctx, repo, true)
}

func (s *PostgresStore) ReadGraphProjectionStatus(ctx context.Context, repo domain.ContentHash) (domain.GraphProjectionRead, error) {
	return s.readGraphProjection(ctx, repo, false)
}

func (s *PostgresStore) readGraphProjection(ctx context.Context, repo domain.ContentHash, includeParts bool) (domain.GraphProjectionRead, error) {
	result, err := s.graphProjection().readWithOptions(ctx, repo, projectionReadOptions{includeParts: includeParts, allowPrior: true, retainFailedOutput: true})
	if err != nil {
		failure := graphProjectionOutputCorruption(ctx, repo, result, err)
		if failure == nil {
			return domain.GraphProjectionRead{}, err
		}
		err = failure
	}
	applied := result.Applied
	if result.Dataset == "" {
		applied = domain.RepositoryViewSourceCheckpoint{}
	}
	return domain.GraphProjectionRead{
		NeedsAttention: result.NeedsAttention, Enabled: result.Enabled, Ready: result.Ready,
		Complete: result.complete, Rules: result.rules, Dataset: result.Dataset,
		Source: result.Source, Applied: applied, Parts: result.Parts,
	}, err
}

func (s *PostgresStore) BuildGraphSourceReader(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, repo domain.Repo, entries []domain.RepositoryViewSourceEntry) (outbound.GraphEvidenceReader, error) {
	return NewGraphSourceReader(ctx, checkpoint, repo, entries)
}
