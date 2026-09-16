//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
	"time"
)

func storageWriteError(err error) error {
	var state interface{ SQLState() string }
	if errors.As(err, &state) && state.SQLState() == "CXT01" {
		return domain.ErrStorageLimit
	}
	return err
}
func (s *PostgresStore) ReconcileStorageUsage(ctx context.Context, ns string) error {
	if domain.ValidateNamespaceID(ns) != nil {
		return domain.ErrValidation
	}
	_, err := s.pool.Exec(ctx, `SELECT cxt_storage_reconcile($1)`, ns)
	return err
}
func (s *PostgresStore) ReadStorageUsage(ctx context.Context, ns string, start, end time.Time) (domain.StorageUsage, error) {
	u := domain.StorageUsage{NamespaceID: ns, PeriodStart: start, PeriodEnd: end, Entries: []domain.StorageUsageEntry{}, OverageByteHours: "0"}
	if domain.ValidateNamespaceID(ns) != nil || !end.After(start) || end.Sub(start) > 32*24*time.Hour {
		return u, domain.ErrValidation
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return u, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `SELECT coalesce(plan,''),included_bytes,payg,max_bytes,grace_bytes,grace_until,current_bytes,policy_revision FROM storage_accounts WHERE namespace_id=$1`, ns).Scan(&u.Policy.Plan, &u.Policy.IncludedBytes, &u.Policy.PayAsYouGo, &u.Policy.MaxBytes, &u.Policy.GraceBytes, &u.Policy.GraceUntil, &u.CurrentBytes, &u.PolicyRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return u, err
	}
	if err = tx.QueryRow(ctx, `SELECT min(occurred_at) FROM storage_usage_ledger WHERE namespace_id=$1`, ns).Scan(&u.MeteredSince); err != nil {
		return u, err
	}
	// Last point before the interval supplies its opening balance. Intermediate
	// changes within one transaction share a timestamp and contribute zero hours.
	err = tx.QueryRow(ctx, `WITH points AS ((SELECT * FROM storage_usage_ledger WHERE namespace_id=$1 AND occurred_at<$2 ORDER BY occurred_at DESC,seq DESC LIMIT 1) UNION ALL SELECT * FROM storage_usage_ledger WHERE namespace_id=$1 AND occurred_at>=$2 AND occurred_at<$3), spans AS (SELECT *,lead(occurred_at,1,$3) OVER(ORDER BY occurred_at,seq) AS until FROM points) SELECT coalesce(sum(CASE WHEN payg THEN greatest(bytes_after-included_bytes,0)::numeric * greatest(extract(epoch FROM (least(until,$3)-greatest(occurred_at,$2))),0)/3600 ELSE 0 END),0)::text FROM spans`, ns, start, end).Scan(&u.OverageByteHours)
	if err != nil {
		return u, err
	}
	rows, err := tx.Query(ctx, `SELECT seq,delta_bytes,bytes_after,reason,occurred_at FROM storage_usage_ledger WHERE namespace_id=$1 ORDER BY seq DESC LIMIT 50`, ns)
	if err != nil {
		return u, err
	}
	for rows.Next() {
		var e domain.StorageUsageEntry
		if err = rows.Scan(&e.Sequence, &e.DeltaBytes, &e.BytesAfter, &e.Reason, &e.OccurredAt); err != nil {
			rows.Close()
			return u, err
		}
		u.Entries = append(u.Entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return u, err
	}
	u.ProjectState(time.Now().UTC())
	return u, tx.Commit(ctx)
}

// Idempotency key and expected revision bind a provisioning command to its exact
// payload. This port is intentionally absent from the customer REST/MCP surface.
func (s *PostgresStore) ConfigureStoragePolicy(ctx context.Context, ns, operation string, expected int64, p domain.StoragePolicy, actor, reason string) error {
	if domain.ValidateNamespaceID(ns) != nil || domain.ValidateExternalID(operation) != nil || strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len(actor) > 256 || len(reason) > 2048 || expected < 0 {
		return domain.ErrValidation
	}
	if err := p.Validate(); err != nil {
		return err
	}
	payload, _ := json.Marshal(struct {
		Policy        domain.StoragePolicy
		Expected      int64
		Actor, Reason string
	}{p, expected, actor, reason})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO storage_accounts(namespace_id) VALUES($1) ON CONFLICT DO NOTHING`, ns); err != nil {
		return err
	}
	var revision int64
	if err = tx.QueryRow(ctx, `SELECT policy_revision FROM storage_accounts WHERE namespace_id=$1 FOR UPDATE`, ns).Scan(&revision); err != nil {
		return err
	}
	var same bool
	err = tx.QueryRow(ctx, `SELECT namespace_id=$2 AND payload=$3::jsonb FROM storage_policy_operations WHERE id=$1`, operation, ns, payload).Scan(&same)
	if err == nil {
		if same {
			return tx.Commit(ctx)
		}
		return domain.ErrConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if revision != expected {
		return domain.ErrRefConflict
	}
	var kind string
	if err = tx.QueryRow(ctx, `SELECT kind FROM namespaces WHERE id=$1`, ns).Scan(&kind); err != nil {
		return err
	}
	if (p.Plan == "enterprise") != (kind == "enterprise") {
		return domain.ErrValidation
	}
	_, err = tx.Exec(ctx, `UPDATE storage_accounts SET plan=$2,included_bytes=$3,payg=$4,max_bytes=$5,grace_bytes=$6,grace_until=$7,policy_revision=policy_revision+1,changed_at=clock_timestamp() WHERE namespace_id=$1`, ns, p.Plan, p.IncludedBytes, p.PayAsYouGo, p.MaxBytes, p.GraceBytes, p.GraceUntil)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO storage_usage_ledger(namespace_id,delta_bytes,bytes_after,included_bytes,payg,plan,reason,occurred_at) SELECT namespace_id,0,current_bytes,included_bytes,payg,plan,'policy.changed',changed_at FROM storage_accounts WHERE namespace_id=$1`, ns)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO storage_policy_operations(id,namespace_id,payload,revision) VALUES($1,$2,$3,$4)`, operation, ns, payload, revision+1)
	if err != nil {
		return mapPGConstraint(err)
	}
	return tx.Commit(ctx)
}

// Independent workers claim distinct account rows; retry after interruption is
// safe because reconciliation records only differences from stored payloads.
func (s *PostgresStore) ReconcileNextStorageUsage(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO storage_accounts(namespace_id) SELECT id FROM namespaces ON CONFLICT DO NOTHING`); err != nil {
		return false, err
	}
	var ns string
	err = tx.QueryRow(ctx, `SELECT namespace_id FROM storage_accounts WHERE reconciled_at IS NULL OR reconciled_at<clock_timestamp()-interval '24 hours' ORDER BY reconciled_at NULLS FIRST,namespace_id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&ns)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `SELECT cxt_storage_reconcile($1)`, ns); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
