//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *PostgresStore) EnqueueNotification(ctx context.Context, d outbound.NotificationDelivery) error {
	b, err := json.Marshal(d.Job)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO notification_outbox(id,repository_id,destination,payload,state,version,created_at,next_attempt,lease_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, d.Job.ID, d.Job.RepositoryID, d.Destination, b, d.Job.State, d.Job.Version, d.Job.CreatedAt, d.Job.NextAttempt, d.Job.LeaseUntil)
	return err
}
func (s *PostgresStore) ListNotifications(ctx context.Context, repository string) ([]domain.NotificationJob, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT payload FROM notification_outbox WHERE repository_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.NotificationJob{}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var j domain.NotificationJob
		if err := json.Unmarshal(b, &j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *PostgresStore) ClaimNotification(ctx context.Context, _ time.Time, lease time.Duration) (outbound.NotificationDelivery, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return outbound.NotificationDelivery{}, err
	}
	defer rollbackPG(tx)
	if err = lockNotificationPolicy(ctx, tx); err != nil {
		return outbound.NotificationDelivery{}, err
	}
	allowed, err := workerDocumentRequirements(ctx)
	if err != nil {
		return outbound.NotificationDelivery{}, err
	}
	var d outbound.NotificationDelivery
	var raw []byte
	var now time.Time
	var storedID, repository string
	err = tx.QueryRow(ctx, `SELECT payload,destination,clock_timestamp(),id,repository_id FROM notification_outbox n WHERE ((state IN ('pending','retrying') AND next_attempt<=clock_timestamp()) OR (state='running' AND lease_until<=clock_timestamp())) AND NOT EXISTS(SELECT 1 FROM repos r WHERE r.repository_id=n.repository_id AND NOT (r.required_doc_identity=ANY($1))) ORDER BY next_attempt,created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, allowed).Scan(&raw, &d.Destination, &now, &storedID, &repository)
	if err != nil {
		return d, mapNoRows(err)
	}
	if err = json.Unmarshal(raw, &d.Job); err != nil {
		return d, err
	}
	if d.Job.ID != storedID || d.Job.RepositoryID != repository {
		return d, domain.ErrIntegrity
	}
	if err = checkNotificationPolicy(ctx, tx, repository); err != nil {
		return d, err
	}
	j := &d.Job
	j.State = "running"
	j.Attempts++
	j.Version++
	j.UpdatedAt = now
	j.LeaseUntil = now.Add(lease)
	raw, err = json.Marshal(j)
	if err != nil {
		return d, err
	}
	_, err = tx.Exec(ctx, `UPDATE notification_outbox SET payload=$2,state=$3,version=$4,lease_until=$5 WHERE id=$1`, j.ID, raw, j.State, j.Version, j.LeaseUntil)
	if err != nil {
		return d, err
	}
	return d, tx.Commit(ctx)
}
func (s *PostgresStore) FinishNotification(ctx context.Context, j domain.NotificationJob, _ time.Time) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	if err = lockNotificationPolicy(ctx, tx); err != nil {
		return err
	}
	var raw []byte
	var repository string
	if err = tx.QueryRow(ctx, `SELECT payload,repository_id FROM notification_outbox WHERE id=$1 FOR UPDATE`, j.ID).Scan(&raw, &repository); err != nil {
		return mapNoRows(err)
	}
	var current domain.NotificationJob
	if err = json.Unmarshal(raw, &current); err != nil {
		return err
	}
	if current.RepositoryID != repository || !notificationPayloadMatches(current, j) {
		return domain.ErrRefConflict
	}
	if err = checkNotificationPolicy(ctx, tx, current.RepositoryID); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE notification_outbox SET payload=$3,state=$4,next_attempt=$5,lease_until=$6 WHERE id=$1 AND version=$2 AND state='running' AND lease_until>clock_timestamp()`, j.ID, j.Version, b, j.State, j.NextAttempt, j.LeaseUntil)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrRefConflict
	}
	return tx.Commit(ctx)
}
func (s *PostgresStore) RetryNotification(ctx context.Context, repository, id, destination string, _ time.Time) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	var raw []byte
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT payload,clock_timestamp() FROM notification_outbox WHERE id=$1 AND repository_id=$2 FOR UPDATE`, id, repository).Scan(&raw, &now); err != nil {
		return mapNoRows(err)
	}
	var j domain.NotificationJob
	if err = json.Unmarshal(raw, &j); err != nil {
		return err
	}
	if j.State == "delivered" || (j.State == "running" && j.LeaseUntil.After(now)) {
		return domain.ErrConflict
	}
	j.State = "pending"
	j.Version++
	j.Attempts = 0
	j.Reason = ""
	j.HTTPStatus = 0
	j.NextAttempt = now
	j.LeaseUntil = time.Time{}
	j.UpdatedAt = now
	raw, err = json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE notification_outbox SET destination=$2,payload=$3,state=$4,version=$5,next_attempt=$6,lease_until=$7 WHERE id=$1`, j.ID, destination, raw, j.State, j.Version, j.NextAttempt, j.LeaseUntil)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) WithinRepositoryMetadata(ctx context.Context, repository string, fn func(context.Context) error) error {
	if prior, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx); ok {
		if prior.owner != s || prior.readOnly || (!prior.identity && prior.repository != repository) {
			return domain.ErrConflict
		}
		return fn(ctx)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cxt-identity-access',0))`); err != nil {
		return err
	}
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM repositories WHERE id=$1 FOR UPDATE`, repository).Scan(&id); err != nil {
		return mapNoRows(err)
	}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: tx, owner: s, repository: repository})
	if err = fn(bound); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var _ outbound.NotificationStore = (*PostgresStore)(nil)
var _ outbound.RepositoryMetadataTransactions = (*PostgresStore)(nil)

func (s *PostgresStore) ValidateNotificationDelivery(ctx context.Context, claimed outbound.NotificationDelivery, _ time.Time) error {
	return s.WithinIdentity(ctx, func(ctx context.Context) error {
		tx := s.db(ctx)
		var current outbound.NotificationDelivery
		var raw []byte
		var repository string
		if err := tx.QueryRow(ctx, `SELECT payload,destination,repository_id FROM notification_outbox WHERE id=$1 FOR UPDATE`, claimed.Job.ID).Scan(&raw, &current.Destination, &repository); err != nil {
			return mapNoRows(err)
		}
		if err := json.Unmarshal(raw, &current.Job); err != nil {
			return err
		}
		if current.Job.ID != claimed.Job.ID || current.Job.RepositoryID != repository {
			return domain.ErrIntegrity
		}
		if err := checkNotificationPolicy(ctx, tx, repository); err != nil {
			return err
		}
		repo, err := s.GetRepository(ctx, current.Job.RepositoryID)
		if err != nil {
			return err
		}
		// Sample the database clock after every policy/configuration wait,
		// while the identity transaction and current claim remain pinned.
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		return checkNotificationDelivery(current, claimed, repo, now)
	})
}

var _ outbound.NotificationDeliveryGuard = (*PostgresStore)(nil)
