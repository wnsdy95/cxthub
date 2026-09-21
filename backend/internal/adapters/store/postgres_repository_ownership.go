//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.RepositoryBindings = (*PostgresStore)(nil)

func (s *PostgresStore) GetBoundRepo(ctx context.Context, id string) (domain.Repo, error) {
	if err := domain.ValidateRepositoryID(id); err != nil {
		return domain.Repo{}, err
	}
	var repo domain.ContentHash
	if err := s.db(ctx).QueryRow(ctx, `SELECT id FROM repos WHERE repository_id=$1`, id).Scan(&repo); err != nil {
		return domain.Repo{}, mapNoRows(err)
	}
	return s.GetRepo(ctx, repo)
}
func (s *PostgresStore) InviteTargets(ctx context.Context, token string) ([]string, error) {
	inv, err := s.GetInvite(ctx, token)
	if err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT repository_id FROM repository_invite_targets WHERE token=$1 ORDER BY repository_id`, token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		targets = append(targets, id)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return []string{inv.RepositoryID}, nil
	}
	return targets, nil
}

func (s *PostgresStore) repositoryAlias(ctx context.Context, key, path string) (domain.RepositoryPathAlias, error) {
	var alias domain.RepositoryPathAlias
	err := s.db(ctx).QueryRow(ctx, `SELECT owner_handle,path,repository_id,COALESCE(context_repo_id,'') FROM repository_path_aliases WHERE namespace_key=$1 AND path=$2`, key, path).Scan(&alias.Owner, &alias.Path, &alias.RepositoryID, &alias.ContextRepoID)
	return alias, mapNoRows(err)
}
