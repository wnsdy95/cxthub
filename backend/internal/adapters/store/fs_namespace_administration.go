package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *FSStore) RenameOrganizationNamespace(ctx context.Context, id, next string) error {
	org, err := s.GetOrganization(ctx, id)
	if err != nil {
		return err
	}
	if err = s.RenameNamespace(ctx, org.NamespaceID, next); err != nil {
		return err
	}
	org.Slug = next
	raw, err := json.Marshal(org)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.organizationsDir(), id+".json"), raw)
}
func (s *FSStore) RenameEnterpriseSlug(ctx context.Context, id, next string) error {
	if !domain.ValidNamespaceSlug(next) {
		return domain.ErrValidation
	}
	a, err := s.readEnterpriseAccount(id)
	if err != nil {
		return err
	}
	claimed, err := s.GetEnterpriseBySlug(ctx, next)
	if err == nil && claimed.ID != id {
		return domain.ErrConflict
	}
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	a.Aliases = append(a.Aliases, a.Enterprise.Slug)
	a.Enterprise.Slug = next
	return s.writeEnterpriseAccount(a)
}
