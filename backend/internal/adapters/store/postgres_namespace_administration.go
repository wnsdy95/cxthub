//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) RenameOrganizationNamespace(ctx context.Context, id, next string) error {
	org, err := s.GetOrganization(ctx, id)
	if err != nil {
		return err
	}
	if err = s.RenameNamespace(ctx, org.NamespaceID, next); err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `UPDATE organizations SET slug=$2 WHERE id=$1`, id, next)
	return mapPGConstraint(err)
}
func (s *PostgresStore) RenameEnterpriseSlug(ctx context.Context, id, next string) error {
	if !domain.ValidNamespaceSlug(next) {
		return domain.ErrValidation
	}
	e, err := s.GetEnterprise(ctx, id)
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
	_, err = s.db(ctx).Exec(ctx, `INSERT INTO enterprise_slug_aliases(slug,enterprise_id) VALUES($1,$2) ON CONFLICT(slug) DO NOTHING`, e.Slug, id)
	if err != nil {
		return err
	}
	e.Slug = next
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `UPDATE enterprises SET slug=$2,record=$3 WHERE id=$1`, id, next, raw)
	return mapPGConstraint(err)
}
