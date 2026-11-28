//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.AnchoredSummaryStore = (*PostgresStore)(nil)

const anchoredSummaryColumns = `repo_id,provider,session_id,commit_sha,body,body_hash,created_at,updated_at`

func scanAnchoredSummary(row pgx.Row) (domain.AnchoredSummary, error) {
	var summary domain.AnchoredSummary
	var body []byte
	if err := row.Scan(&summary.RepoID, &summary.Provider, &summary.SessionID, &summary.Commit, &body, &summary.BodyHash, &summary.CreatedAt, &summary.UpdatedAt); err != nil {
		return domain.AnchoredSummary{}, err
	}
	if err := json.Unmarshal(body, &summary.Body); err != nil {
		return domain.AnchoredSummary{}, fmt.Errorf("%w: invalid stored summary: %v", domain.ErrIntegrity, err)
	}
	summary.Body = summary.Body.Canonical()
	summary.CreatedAt, summary.UpdatedAt = summary.CreatedAt.UTC(), summary.UpdatedAt.UTC()
	if err := summary.Validate(); err != nil {
		return domain.AnchoredSummary{}, fmt.Errorf("%w: invalid stored summary", domain.ErrIntegrity)
	}
	return summary, nil
}

func (s *PostgresStore) PutAnchoredSummary(ctx context.Context, summary domain.AnchoredSummary) error {
	if err := summary.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(summary.Body.Canonical())
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO anchored_session_summaries(`+anchoredSummaryColumns+`) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
 ON CONFLICT (repo_id,provider,session_id,commit_sha) DO UPDATE SET body=EXCLUDED.body, body_hash=EXCLUDED.body_hash, updated_at=EXCLUDED.updated_at`,
		summary.RepoID, summary.Provider, summary.SessionID, summary.Commit, body, summary.BodyHash, summary.CreatedAt.UTC(), summary.UpdatedAt.UTC())
	return err
}

func (s *PostgresStore) GetAnchoredSummary(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID, commit string) (domain.AnchoredSummary, error) {
	if err := validateHash(repo); err != nil {
		return domain.AnchoredSummary{}, err
	}
	summary, err := scanAnchoredSummary(s.db(ctx).QueryRow(ctx, `SELECT `+anchoredSummaryColumns+` FROM anchored_session_summaries
 WHERE repo_id=$1 AND provider=$2 AND session_id=$3 AND commit_sha=$4`, repo, provider, sessionID, commit))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AnchoredSummary{}, domain.ErrNotFound
	}
	return summary, err
}

func (s *PostgresStore) ListCommitAnchoredSummaries(ctx context.Context, repo domain.ContentHash, commits []string) ([]domain.AnchoredSummary, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	listed := []domain.AnchoredSummary{}
	if len(commits) == 0 {
		return listed, nil
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT `+anchoredSummaryColumns+` FROM anchored_session_summaries
 WHERE repo_id=$1 AND commit_sha = ANY($2) ORDER BY commit_sha, provider, session_id`, repo, commits)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		summary, err := scanAnchoredSummary(rows)
		if err != nil {
			return nil, err
		}
		listed = append(listed, summary)
	}
	return listed, rows.Err()
}
