package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type ProviderSessionInventory interface {
	LocateActiveSession(context.Context, string, domain.ProviderKind) (string, error)
	SessionFilesForCwd(context.Context, string, domain.ProviderKind) []string
	MatchesSession(string, domain.ProviderKind, string, string) bool
}
