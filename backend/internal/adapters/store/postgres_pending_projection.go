//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.PendingProjectionStore = (*PostgresStore)(nil)
var _ outbound.PendingProjectionStatusStore = (*PostgresStore)(nil)

const pendingProjectionWorkColumns = projectionWorkColumns

type pendingProjectionWork = projectionWork
type pendingProjectionSource = projectionSource

func (s *PostgresStore) pendingProjection() projectionStore {
	return projectionStore{PostgresStore: s, spec: projectionSpec{
		scope: domain.RepositoryViewSourceScope,
		rules: domain.PendingProjectionRules,
		work:  "pending_projection_work", inputs: "pending_projection_inputs",
		parts: "pending_projection_parts", ready: "pending_projection_ready",
		sourceSQL: `SELECT catalog.epoch::text,catalog.head_seq,catalog.floor_seq,source.epoch::text,source.baseline_seq
   FROM repository_catalog_state catalog JOIN repository_view_source_state source USING(repo_id)
   WHERE catalog.repo_id=$1`,
		kindValid: domain.RepositoryViewSourceKindValid,
		decodeParts: func(ctx context.Context, repo domain.ContentHash, parts []domain.PendingProjectionPart) error {
			_, err := domain.DecodePendingProjection(ctx, repo, parts)
			return err
		},
		partKeys: pendingProjectionReadyPartKeys,
	}}
}

func (s *PostgresStore) ConfigurePendingProjection(ctx context.Context, repo domain.ContentHash, enabled bool) error {
	return s.pendingProjection().configure(ctx, repo, enabled)
}

func (s *PostgresStore) ClaimPendingProjection(ctx context.Context, duration time.Duration) (domain.PendingProjectionLease, error) {
	return s.pendingProjection().claim(ctx, duration)
}

func (s *PostgresStore) RenewPendingProjection(ctx context.Context, lease domain.PendingProjectionLease, duration time.Duration) error {
	return s.pendingProjection().renew(ctx, lease, duration)
}

func (s *PostgresStore) SavePendingProjectionPage(ctx context.Context, lease domain.PendingProjectionLease, page domain.RepositoryViewSourcePage) error {
	return s.pendingProjection().savePage(ctx, lease, page)
}

func (s *PostgresStore) LoadPendingProjectionInputs(ctx context.Context, lease domain.PendingProjectionLease) ([]domain.RepositoryViewSourceEntry, error) {
	return s.pendingProjection().loadInputs(ctx, lease)
}

func (s *PostgresStore) PublishPendingProjection(ctx context.Context, lease domain.PendingProjectionLease, parts []domain.PendingProjectionPart) error {
	return s.pendingProjection().publish(ctx, lease, parts)
}

func (s *PostgresStore) FailPendingProjection(ctx context.Context, lease domain.PendingProjectionLease, reason string, retry bool) error {
	return s.pendingProjection().fail(ctx, lease, reason, retry)
}

func (s *PostgresStore) ReadPendingProjection(ctx context.Context, repo domain.ContentHash) (domain.PendingProjectionRead, error) {
	return s.pendingProjection().read(ctx, repo, true)
}

func (s *PostgresStore) ReadPendingProjectionStatus(ctx context.Context, repo domain.ContentHash) (domain.PendingProjectionRead, error) {
	return s.pendingProjection().read(ctx, repo, false)
}

func pendingProjectionReadyPartKeys(header domain.PendingProjectionPart) ([]string, []string, error) {
	var index struct {
		Order map[string][]string `json:"order"`
	}
	if header.Kind != "header" || header.Key != "view" || json.Unmarshal(header.Value, &index) != nil || len(index.Order) != 5 {
		return nil, nil, domain.ErrIntegrity
	}
	count := 1
	for _, keys := range index.Order {
		count += len(keys)
	}
	if count > domain.MaxPendingProjectionOutputRows {
		return nil, nil, domain.ErrRepositoryViewSourceLimit
	}
	kinds, keys := make([]string, 0, count-1), make([]string, 0, count-1)
	seen := make(map[[2]string]bool, count-1)
	for _, kind := range []string{"archive", "hold", "pending", "ref", "snapshot"} {
		identities, exists := index.Order[kind]
		if !exists {
			return nil, nil, domain.ErrIntegrity
		}
		for _, key := range identities {
			identity := [2]string{kind, key}
			if key == "" || len(key) > domain.MaxRepositoryViewSourceKeyBytes || seen[identity] {
				return nil, nil, domain.ErrIntegrity
			}
			seen[identity] = true
			kinds, keys = append(kinds, kind), append(keys, key)
		}
	}
	return kinds, keys, nil
}

func pendingProjectionCheckpoint(repo domain.ContentHash, epoch string, sequence int64) domain.RepositoryViewSourceCheckpoint {
	return (*PostgresStore)(nil).pendingProjection().checkpoint(repo, epoch, sequence)
}

func scanPendingProjectionWork(row pgx.Row) (pendingProjectionWork, error) {
	return scanProjectionWork(row)
}

func readPendingProjectionSource(ctx context.Context, database catalogMerkleReader, repo domain.ContentHash, lock bool) (pendingProjectionSource, error) {
	return (*PostgresStore)(nil).pendingProjection().readSource(ctx, database, repo, lock)
}

func pendingProjectionLeaseValid(lease domain.PendingProjectionLease) bool {
	return (*PostgresStore)(nil).pendingProjection().leaseValid(lease)
}

func pendingProjectionLeaseMatches(work pendingProjectionWork, lease domain.PendingProjectionLease) bool {
	return projectionLeaseMatches(work, lease)
}

func validatePendingProjectionPage(work pendingProjectionWork, source pendingProjectionSource, page domain.RepositoryViewSourcePage) error {
	return (*PostgresStore)(nil).pendingProjection().validatePage(work, source, page)
}

func validatePendingProjectionParts(ctx context.Context, repo domain.ContentHash, parts []domain.PendingProjectionPart) error {
	return (*PostgresStore)(nil).pendingProjection().validateParts(ctx, repo, parts)
}

func (s *PostgresStore) readPendingProjectionReadyParts(ctx context.Context, repo domain.ContentHash, dataset string, sequence int64) ([]domain.PendingProjectionPart, error) {
	return s.pendingProjection().readReadyParts(ctx, repo, dataset, sequence)
}
