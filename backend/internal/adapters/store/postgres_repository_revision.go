//go:build postgres

package store

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	var v domain.RepositoryRevision
	err := s.db(ctx).QueryRow(ctx, `SELECT graph,pending,evidence FROM repository_revisions WHERE repo_id=$1`, repo).Scan(&v.Graph, &v.Pending, &v.Evidence)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return v, err
}
func (s *PostgresStore) AdvanceRepositoryRevision(ctx context.Context, repo domain.ContentHash, pending bool) error {
	g, p := 1, 0
	if pending {
		g, p = 0, 1
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO repository_revisions(repo_id,graph,pending) VALUES($1,$2,$3) ON CONFLICT(repo_id) DO UPDATE SET graph=repository_revisions.graph+EXCLUDED.graph,pending=repository_revisions.pending+EXCLUDED.pending`, repo, g, p)
	return err
}

func (s *PostgresStore) AdvanceEvidenceRevision(ctx context.Context, repo domain.ContentHash) error {
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO repository_revisions(repo_id,graph,pending,evidence) VALUES($1,0,0,1) ON CONFLICT(repo_id) DO UPDATE SET evidence=repository_revisions.evidence+1`, repo)
	return err
}
