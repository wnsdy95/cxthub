package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.UserPreferencesStore = (*FSStore)(nil)

func (storage *FSStore) UpdateUserPreferences(ctx context.Context, userID string, patch domain.UserPreferencesPatch) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	user, err := storage.GetUser(ctx, userID)
	if err != nil {
		return domain.User{}, err
	}
	updated, err := domain.ApplyUserPreferences(user, patch)
	if err != nil {
		return domain.User{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	if err := storage.UpsertUser(ctx, updated); err != nil {
		return domain.User{}, err
	}
	return updated, nil
}
