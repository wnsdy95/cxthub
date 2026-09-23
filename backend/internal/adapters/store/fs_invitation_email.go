package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.InvitationEmailStore = (*FSStore)(nil)

func (s *FSStore) invitationEmailPath(id string) string {
	return filepath.Join(s.dataDir, "invitation-emails", id+".json")
}
func (s *FSStore) PutInvitationEmail(_ context.Context, j domain.InvitationEmail) error {
	if err := domain.ValidateInvitationEmail(j); err != nil {
		return err
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return writeAtomic(s.invitationEmailPath(j.InvitationID), raw)
}
func (s *FSStore) GetInvitationEmail(_ context.Context, id string) (domain.InvitationEmail, error) {
	var j domain.InvitationEmail
	if domain.ValidateCollaborationInviteID(id) != nil {
		return j, domain.ErrValidation
	}
	if err := readJSON(s.invitationEmailPath(id), &j); err != nil {
		return j, err
	}
	if j.InvitationID != id || domain.ValidateInvitationEmail(j) != nil {
		return domain.InvitationEmail{}, domain.ErrIntegrity
	}
	return j, nil
}
func (s *FSStore) NextInvitationEmail(ctx context.Context) (domain.InvitationEmail, time.Time, error) {
	now := time.Now().UTC()
	var next domain.InvitationEmail
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "invitation-emails"))
	if errors.Is(err, os.ErrNotExist) {
		return next, now, domain.ErrNotFound
	}
	if err != nil {
		return next, now, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		j, err := s.GetInvitationEmail(ctx, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			return next, now, err
		}
		if j.Ready(now) && (next.InvitationID == "" || j.NextAttempt.Before(next.NextAttempt)) {
			next = j
		}
	}
	if next.InvitationID == "" {
		return next, now, domain.ErrNotFound
	}
	return next, now, nil
}
