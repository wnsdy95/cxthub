//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.TeamStore = (*PostgresStore)(nil)
var _ outbound.IdentityTransactions = (*PostgresStore)(nil)

func (s *PostgresStore) WithinIdentity(ctx context.Context, fn func(context.Context) error) error {
	if prior, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx); ok {
		if prior.owner != s || prior.readOnly || !prior.identity {
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
	ctx = context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: tx, owner: s, identity: true})
	if err = fn(ctx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *PostgresStore) CreateTeam(ctx context.Context, t domain.Team) error {
	if err := domain.ValidateTeam(t); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO teams(id,organization_id,name,slug,description,created_at) VALUES($1,$2,$3,$4,$5,$6)`, t.ID, t.OrganizationID, t.Name, t.Slug, t.Description, t.CreatedAt)
	return mapPGConstraint(err)
}
func (s *PostgresStore) GetTeam(ctx context.Context, id string) (domain.Team, error) {
	if err := domain.ValidateTeamID(id); err != nil {
		return domain.Team{}, err
	}
	var t domain.Team
	err := s.db(ctx).QueryRow(ctx, `SELECT id,organization_id,name,slug,description,created_at FROM teams WHERE id=$1`, id).Scan(&t.ID, &t.OrganizationID, &t.Name, &t.Slug, &t.Description, &t.CreatedAt)
	if err != nil {
		return t, mapNoRows(err)
	}
	return t, domain.ValidateTeam(t)
}
func (s *PostgresStore) ListTeams(ctx context.Context, org string) ([]domain.Team, error) {
	if err := domain.ValidateOrganizationID(org); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT id,organization_id,name,slug,description,created_at FROM teams WHERE organization_id=$1 ORDER BY slug`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Team{}
	for rows.Next() {
		var t domain.Team
		if err = rows.Scan(&t.ID, &t.OrganizationID, &t.Name, &t.Slug, &t.Description, &t.CreatedAt); err != nil {
			return nil, err
		}
		if err = domain.ValidateTeam(t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *PostgresStore) DeleteTeam(ctx context.Context, id string) error {
	if err := domain.ValidateTeamID(id); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM teams WHERE id=$1`, id)
	return err
}
func (s *PostgresStore) PutTeamMember(ctx context.Context, m domain.TeamMembership) error {
	if err := domain.ValidateTeamMembership(m); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO team_memberships(team_id,organization_id,user_id,role,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(team_id,user_id) DO UPDATE SET role=EXCLUDED.role`, m.TeamID, m.OrganizationID, m.UserID, m.Role, m.CreatedAt)
	return mapPGConstraint(err)
}
func (s *PostgresStore) RemoveTeamMember(ctx context.Context, team, user string) error {
	if err := domain.ValidateTeamID(team); err != nil {
		return err
	}
	if err := domain.ValidateExternalID(user); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM team_memberships WHERE team_id=$1 AND user_id=$2`, team, user)
	return err
}
func (s *PostgresStore) ListTeamMembers(ctx context.Context, team string) ([]domain.TeamMembership, error) {
	if err := domain.ValidateTeamID(team); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT team_id,organization_id,user_id,role,created_at FROM team_memberships WHERE team_id=$1 ORDER BY user_id`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.TeamMembership{}
	for rows.Next() {
		var m domain.TeamMembership
		if err = rows.Scan(&m.TeamID, &m.OrganizationID, &m.UserID, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		if err = domain.ValidateTeamMembership(m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *PostgresStore) PutTeamRepositoryGrant(ctx context.Context, g domain.TeamRepositoryGrant) error {
	if err := domain.ValidateTeamRepositoryGrant(g); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO team_repository_grants(team_id,organization_id,repository_id,role,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(team_id,repository_id) DO UPDATE SET role=EXCLUDED.role`, g.TeamID, g.OrganizationID, g.RepositoryID, g.Role, g.CreatedAt)
	return mapPGConstraint(err)
}
func (s *PostgresStore) RemoveTeamRepositoryGrant(ctx context.Context, team, repository string) error {
	if err := domain.ValidateTeamID(team); err != nil {
		return err
	}
	if err := domain.ValidateRepositoryID(repository); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM team_repository_grants WHERE team_id=$1 AND repository_id=$2`, team, repository)
	return err
}
func (s *PostgresStore) ListTeamRepositoryGrants(ctx context.Context, team string) ([]domain.TeamRepositoryGrant, error) {
	if err := domain.ValidateTeamID(team); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT team_id,organization_id,repository_id,role,created_at FROM team_repository_grants WHERE team_id=$1 ORDER BY repository_id`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.TeamRepositoryGrant{}
	for rows.Next() {
		var g domain.TeamRepositoryGrant
		if err = rows.Scan(&g.TeamID, &g.OrganizationID, &g.RepositoryID, &g.Role, &g.CreatedAt); err != nil {
			return nil, err
		}
		if err = domain.ValidateTeamRepositoryGrant(g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
func (s *PostgresStore) RepositoryTeamAccess(ctx context.Context, repository, user string) ([]domain.TeamRepositoryAccess, error) {
	if err := domain.ValidateRepositoryID(repository); err != nil {
		return nil, err
	}
	if err := domain.ValidateExternalID(user); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT g.team_id,g.repository_id,o.namespace_id,m.user_id,g.role FROM team_repository_grants g JOIN teams t ON t.id=g.team_id AND t.organization_id=g.organization_id JOIN organizations o ON o.id=t.organization_id JOIN team_memberships m ON m.team_id=t.id AND m.organization_id=t.organization_id JOIN organization_memberships om ON om.organization_id=t.organization_id AND om.user_id=m.user_id WHERE g.repository_id=$1 AND m.user_id=$2`, repository, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.TeamRepositoryAccess{}
	for rows.Next() {
		var g domain.TeamRepositoryAccess
		if err = rows.Scan(&g.TeamID, &g.RepositoryID, &g.OrganizationNamespaceID, &g.UserID, &g.Role); err != nil {
			return nil, err
		}
		g.TeamMember = true
		g.OrganizationMember = true
		out = append(out, g)
	}
	return out, rows.Err()
}
