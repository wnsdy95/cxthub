package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type CommandSessionSelection struct {
	SessionID   string
	Provider    domain.ProviderKind
	SessionPath string
}

type CommandSessionResolver interface {
	ResolveCommandSession(context.Context, string, domain.ProviderKind) (CommandSessionSelection, error)
	SupervisedProvider(context.Context, string) (domain.ProviderKind, bool)
}
