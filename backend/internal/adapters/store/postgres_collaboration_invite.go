//go:build postgres

package store

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.CollaborationInviteStore = (*PostgresStore)(nil)

func (s *PostgresStore) PutCollaborationInvite(ctx context.Context, invite domain.CollaborationInvite) error {
	if err := domain.ValidateCollaborationInvite(invite); err != nil {
		return err
	}
	raw, err := json.Marshal(invite)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO collaboration_invites(id,kind,space_id,email,record) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO UPDATE SET record=EXCLUDED.record`, invite.ID, invite.Kind, invite.SpaceID, invite.Email, raw)
	return mapPGConstraint(err)
}
func (s *PostgresStore) GetCollaborationInvite(ctx context.Context, id string) (domain.CollaborationInvite, error) {
	var invite domain.CollaborationInvite
	if err := domain.ValidateCollaborationInviteID(id); err != nil {
		return invite, err
	}
	var raw []byte
	if err := s.db(ctx).QueryRow(ctx, `SELECT record FROM collaboration_invites WHERE id=$1`, id).Scan(&raw); err != nil {
		return invite, mapNoRows(err)
	}
	if json.Unmarshal(raw, &invite) != nil || invite.ID != id || domain.ValidateCollaborationInvite(invite) != nil {
		return domain.CollaborationInvite{}, domain.ErrIntegrity
	}
	return invite, nil
}
func (s *PostgresStore) ListCollaborationInvites(ctx context.Context, kind, space, email string) ([]domain.CollaborationInvite, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT record FROM collaboration_invites WHERE ($3<>'' AND email=$3) OR ($3='' AND kind=$1 AND space_id=$2) ORDER BY record->>'created_at' DESC,id DESC`, kind, space, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.CollaborationInvite{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var i domain.CollaborationInvite
		if json.Unmarshal(raw, &i) != nil || domain.ValidateCollaborationInvite(i) != nil {
			return nil, domain.ErrIntegrity
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
