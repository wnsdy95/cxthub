package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.CollaborationInviteStore = (*FSStore)(nil)

func (s *FSStore) collaborationInvitePath(id string) string {
	return filepath.Join(s.dataDir, "collaboration-invites", id+".json")
}
func (s *FSStore) PutCollaborationInvite(_ context.Context, invite domain.CollaborationInvite) error {
	if err := domain.ValidateCollaborationInvite(invite); err != nil {
		return err
	}
	raw, err := json.Marshal(invite)
	if err != nil {
		return err
	}
	return writeAtomic(s.collaborationInvitePath(invite.ID), raw)
}
func (s *FSStore) GetCollaborationInvite(_ context.Context, id string) (domain.CollaborationInvite, error) {
	var invite domain.CollaborationInvite
	if err := domain.ValidateCollaborationInviteID(id); err != nil {
		return invite, err
	}
	if err := readJSON(s.collaborationInvitePath(id), &invite); err != nil {
		return invite, err
	}
	if invite.ID != id || domain.ValidateCollaborationInvite(invite) != nil {
		return domain.CollaborationInvite{}, domain.ErrIntegrity
	}
	return invite, nil
}
func (s *FSStore) ListCollaborationInvites(ctx context.Context, kind, space, email string) ([]domain.CollaborationInvite, error) {
	out := []domain.CollaborationInvite{}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "collaboration-invites"))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		i, err := s.GetCollaborationInvite(ctx, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if (email != "" && i.Email == email) || (email == "" && i.Kind == kind && i.SpaceID == space) {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}
