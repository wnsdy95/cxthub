//go:build postgres

package store

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitHubStore = (*PostgresStore)(nil)

func githubPGGet[T any](ctx context.Context, s *PostgresStore, table, id string) (v T, err error) {
	var raw []byte
	err = s.db(ctx).QueryRow(ctx, "SELECT body FROM "+table+" WHERE id=$1", id).Scan(&raw)
	if err != nil {
		return v, mapNoRows(err)
	}
	err = json.Unmarshal(raw, &v)
	return
}
func githubPGList[T any](ctx context.Context, s *PostgresStore, table string) ([]T, error) {
	return githubPGQuery[T](ctx, s, "SELECT body FROM "+table+" ORDER BY id")
}
func githubPGQuery[T any](ctx context.Context, s *PostgresStore, query string) ([]T, error) {
	rows, err := s.db(ctx).Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var raw []byte
		var v T
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *PostgresStore) githubWrite(ctx context.Context, table, id string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, "INSERT INTO "+table+"(id,body) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET body=EXCLUDED.body", id, raw)
	return mapPGConstraint(err)
}
func (s *PostgresStore) GetGitHubConnection(ctx context.Context, id string) (domain.GitHubConnection, error) {
	return githubPGGet[domain.GitHubConnection](ctx, s, "github_connections", id)
}
func (s *PostgresStore) PutGitHubConnection(ctx context.Context, v domain.GitHubConnection) error {
	return s.githubWrite(ctx, "github_connections", v.NamespaceID, v)
}
func (s *PostgresStore) ListGitHubConnections(ctx context.Context) ([]domain.GitHubConnection, error) {
	return githubPGList[domain.GitHubConnection](ctx, s, "github_connections")
}
func (s *PostgresStore) GetGitHubRequest(ctx context.Context, id string) (domain.GitHubConnectRequest, error) {
	return githubPGGet[domain.GitHubConnectRequest](ctx, s, "github_requests", id)
}
func (s *PostgresStore) PutGitHubRequest(ctx context.Context, v domain.GitHubConnectRequest) error {
	return s.githubWrite(ctx, "github_requests", v.Hash, v)
}
func (s *PostgresStore) GetGitHubDelivery(ctx context.Context, id string) (domain.GitHubDelivery, error) {
	return githubPGGet[domain.GitHubDelivery](ctx, s, "github_deliveries", id)
}
func (s *PostgresStore) PutGitHubDelivery(ctx context.Context, v domain.GitHubDelivery) error {
	return s.githubWrite(ctx, "github_deliveries", v.ID, v)
}
func (s *PostgresStore) ListGitHubDeliveries(ctx context.Context) ([]domain.GitHubDelivery, error) {
	return githubPGQuery[domain.GitHubDelivery](ctx, s, `SELECT body FROM github_deliveries WHERE NOT (body->>'Done')::boolean AND (body->>'NextAttempt')::timestamptz <= now() AND (body->>'LeaseUntil')::timestamptz <= now() ORDER BY id LIMIT 100`)
}
func (s *PostgresStore) ListGitHubIdentities(ctx context.Context) ([]domain.GitHubIdentity, error) {
	return githubPGList[domain.GitHubIdentity](ctx, s, "github_identities")
}
func (s *PostgresStore) PutGitHubIdentity(ctx context.Context, v domain.GitHubIdentity) error {
	if v.ExternalID <= 0 || domain.ValidateExternalID(v.UserID) != nil {
		return domain.ErrValidation
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r, err := s.db(ctx).Exec(ctx, `INSERT INTO github_identities(id,body) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET body=EXCLUDED.body WHERE github_identities.body->>'external_id'=EXCLUDED.body->>'external_id'`, v.UserID, raw)
	if err != nil {
		return mapPGConstraint(err)
	}
	if r.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
func (s *PostgresStore) ReplaceGitHubTeamMembers(ctx context.Context, ns string, v []domain.GitHubTeamMember) error {
	if domain.ValidateNamespaceID(ns) != nil {
		return domain.ErrValidation
	}
	if _, err := s.db(ctx).Exec(ctx, "DELETE FROM github_team_members WHERE namespace_id=$1", ns); err != nil {
		return err
	}
	for _, m := range v {
		if m.NamespaceID != ns {
			return domain.ErrValidation
		}
		if _, err := s.db(ctx).Exec(ctx, `INSERT INTO github_team_members(namespace_id,team_id,organization_id,user_id,expires_at) VALUES($1,$2,(SELECT organization_id FROM teams WHERE id=$2),$3,$4)`, ns, m.TeamID, m.UserID, m.ExpiresAt); err != nil {
			return mapPGConstraint(err)
		}
	}
	return nil
}
