package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *FSStore) DeleteOAuthClientCodes(_ context.Context, user, client string) error {
	lock := s.oauthLock()
	lock.Lock()
	defer lock.Unlock()
	entries, err := os.ReadDir(s.oauthCodesDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(s.oauthCodesDir(), entry.Name())
		var code domain.OAuthAuthorizationCode
		if err := readJSON(path, &code); err != nil {
			return err
		}
		if code.UserID == user && code.ClientID == client {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *FSStore) AppendAccountAudit(_ context.Context, e domain.AccountAuditEvent) error {
	if err := domain.ValidateAccountAudit(e); err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dataDir, "account-audit", opaqueName(e.UserID), e.ID+".json"), raw)
}
func (s *FSStore) ListAccountAudit(_ context.Context, user string) ([]domain.AccountAuditEvent, error) {
	if domain.ValidateExternalID(user) != nil {
		return nil, domain.ErrValidation
	}
	out := []domain.AccountAuditEvent{}
	dir := filepath.Join(s.dataDir, "account-audit", opaqueName(user))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		var e domain.AccountAuditEvent
		if err = readJSON(filepath.Join(dir, entry.Name()), &e); err != nil {
			return nil, err
		}
		if domain.ValidateAccountAudit(e) != nil || e.UserID != user {
			return nil, domain.ErrIntegrity
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > 100 {
		out = out[:100]
	}
	return out, nil
}
