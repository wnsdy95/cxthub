//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.InvitationEmailStore = (*PostgresStore)(nil)

func (s *PostgresStore) PutInvitationEmail(ctx context.Context, j domain.InvitationEmail) error {
	if err := domain.ValidateInvitationEmail(j); err != nil {
		return err
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO invitation_emails(invitation_id,state,next_attempt,record) VALUES($1,$2,$3,$4)
 ON CONFLICT(invitation_id) DO UPDATE SET state=EXCLUDED.state,next_attempt=EXCLUDED.next_attempt,record=EXCLUDED.record`, j.InvitationID, j.State, j.NextAttempt, raw)
	return mapPGConstraint(err)
}
func decodeInvitationEmail(raw []byte) (domain.InvitationEmail, error) {
	var j domain.InvitationEmail
	if json.Unmarshal(raw, &j) != nil || domain.ValidateInvitationEmail(j) != nil {
		return j, domain.ErrIntegrity
	}
	return j, nil
}
func (s *PostgresStore) GetInvitationEmail(ctx context.Context, id string) (domain.InvitationEmail, error) {
	if domain.ValidateCollaborationInviteID(id) != nil {
		return domain.InvitationEmail{}, domain.ErrValidation
	}
	var raw []byte
	if err := s.db(ctx).QueryRow(ctx, `SELECT record FROM invitation_emails WHERE invitation_id=$1`, id).Scan(&raw); err != nil {
		return domain.InvitationEmail{}, mapNoRows(err)
	}
	j, err := decodeInvitationEmail(raw)
	if err == nil && j.InvitationID != id {
		err = domain.ErrIntegrity
	}
	return j, err
}
func (s *PostgresStore) NextInvitationEmail(ctx context.Context) (domain.InvitationEmail, time.Time, error) {
	var raw []byte
	var now time.Time
	err := s.db(ctx).QueryRow(ctx, `SELECT record,clock_timestamp() FROM invitation_emails WHERE state IN ('queued','retrying','sending') AND next_attempt<=clock_timestamp() ORDER BY next_attempt,invitation_id LIMIT 1 FOR UPDATE`).Scan(&raw, &now)
	if err != nil {
		return domain.InvitationEmail{}, now, mapNoRows(err)
	}
	j, err := decodeInvitationEmail(raw)
	return j, now, err
}
