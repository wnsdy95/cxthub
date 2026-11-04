package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type UserPreferencesStore interface {
	UpdateUserPreferences(context.Context, string, domain.UserPreferencesPatch) (domain.User, error)
}
