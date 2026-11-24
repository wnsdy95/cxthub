//go:build postgres

package store

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.CollectorStore = (*PostgresStore)(nil)

func (s *PostgresStore) PutCollectorReport(ctx context.Context, repo domain.ContentHash, collector domain.ReportedCollector, claims []domain.ClaimedPush) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	if err := validateCollectorReport(collector, claims); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO reported_collectors(repo_id,account,machine,cxt_version,run_error,heard_at) VALUES($1,$2,$3,$4,$5,$6)
 ON CONFLICT (repo_id,account,machine) DO UPDATE SET cxt_version=EXCLUDED.cxt_version, run_error=EXCLUDED.run_error, heard_at=EXCLUDED.heard_at`,
		repo, collector.Account, collector.Machine, collector.Version, collector.RunError, collector.HeardAt.UTC()); err != nil {
		return err
	}
	branches, shas := make([]string, 0, len(claims)), make([]string, 0, len(claims))
	accounts, machines := make([]string, 0, len(claims)), make([]string, 0, len(claims))
	pushed, claimed, clean := make([]time.Time, 0, len(claims)), make([]time.Time, 0, len(claims)), make([]bool, 0, len(claims))
	for _, claim := range claims {
		branches, shas = append(branches, claim.Branch), append(shas, claim.SHA)
		accounts, machines = append(accounts, claim.Account), append(machines, claim.Machine)
		pushed, claimed, clean = append(pushed, claim.PushedAt.UTC()), append(claimed, claim.ClaimedAt.UTC()), append(clean, claim.Clean)
	}
	// The rule of domain.MergeClaimedPush, said in SQL.
	if _, err := tx.Exec(ctx, `INSERT INTO claimed_pushes(repo_id,branch,sha,account,machine,pushed_at,claimed_at,clean)
 SELECT $1, c.branch, c.sha, c.account, c.machine, c.pushed_at, c.claimed_at, c.clean
 FROM unnest($2::text[],$3::text[],$4::text[],$5::text[],$6::timestamptz[],$7::timestamptz[],$8::boolean[]) AS c(branch,sha,account,machine,pushed_at,claimed_at,clean)
 ON CONFLICT (repo_id,branch,sha) DO UPDATE SET clean = claimed_pushes.clean OR EXCLUDED.clean`,
		repo, branches, shas, accounts, machines, pushed, claimed, clean); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) ListReportedCollectors(ctx context.Context, repo domain.ContentHash) ([]domain.ReportedCollector, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT account, machine, cxt_version, run_error, heard_at FROM reported_collectors WHERE repo_id=$1 ORDER BY account, machine`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	collectors := []domain.ReportedCollector{}
	for rows.Next() {
		var collector domain.ReportedCollector
		if err := rows.Scan(&collector.Account, &collector.Machine, &collector.Version, &collector.RunError, &collector.HeardAt); err != nil {
			return nil, err
		}
		collector.HeardAt = collector.HeardAt.UTC()
		collectors = append(collectors, collector)
	}
	return collectors, rows.Err()
}

func (s *PostgresStore) ListClaimedPushes(ctx context.Context, repo domain.ContentHash, limit int) ([]domain.ClaimedPush, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	if limit < 0 {
		limit = 0
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT branch, sha, account, machine, pushed_at, claimed_at, clean FROM claimed_pushes
 WHERE repo_id=$1 ORDER BY pushed_at DESC, branch, sha LIMIT $2`, repo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	claims := []domain.ClaimedPush{}
	for rows.Next() {
		var claim domain.ClaimedPush
		if err := rows.Scan(&claim.Branch, &claim.SHA, &claim.Account, &claim.Machine, &claim.PushedAt, &claim.ClaimedAt, &claim.Clean); err != nil {
			return nil, err
		}
		claim.PushedAt, claim.ClaimedAt = claim.PushedAt.UTC(), claim.ClaimedAt.UTC()
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}
