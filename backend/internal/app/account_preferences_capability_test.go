package app

import (
	"context"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type productionWithoutAtomicUserPreferences struct {
	outbound.ProductionStore
	UpdateUserPreferences int
}

var _ interface {
	UpdateUserPreferences(context.Context, string, domain.UserPreferencesPatch) (domain.User, error)
} = (outbound.ProductionStore)(nil)

func TestProductionStoreRequiresAtomicUserPreferences(test *testing.T) {
	missing := productionWithoutAtomicUserPreferences{}
	if _, supported := any(missing).(outbound.UserPreferencesStore); supported {
		test.Fatal("hidden UpdateUserPreferences still advertises atomic user preferences")
	}
	if err := ValidateProductionStore(missing); err == nil || !strings.Contains(err.Error(), "atomic user preferences") {
		test.Fatalf("production store without atomic user preferences returned %v", err)
	}
	if err := ValidateProductionStore(struct{ outbound.ProductionStore }{}); err != nil {
		test.Fatalf("complete production capability boundary was rejected: %v", err)
	}
}
