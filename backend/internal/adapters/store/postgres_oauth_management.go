//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) DeleteOAuthClientCodes(ctx context.Context, user, client string) error {
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM oauth_authorization_codes WHERE user_id=$1 AND client_id=$2`, user, client)
	return err
}
func (s *PostgresStore) AppendAccountAudit(ctx context.Context, e domain.AccountAuditEvent) error {
	if err := domain.ValidateAccountAudit(e); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO account_audit(id,user_id,client_id,action,created_at) VALUES($1,$2,$3,$4,$5)`, e.ID, e.UserID, e.ClientID, e.Action, e.CreatedAt)
	return err
}
func (s *PostgresStore) ListAccountAudit(ctx context.Context, user string) ([]domain.AccountAuditEvent, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT id,user_id,client_id,action,created_at FROM account_audit WHERE user_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AccountAuditEvent{}
	for rows.Next() {
		var e domain.AccountAuditEvent
		if err = rows.Scan(&e.ID, &e.UserID, &e.ClientID, &e.Action, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
