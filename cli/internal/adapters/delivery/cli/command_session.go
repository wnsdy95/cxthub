package cli

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type commandCaptureTarget = outbound.CommandSessionSelection

func (container *Container) commandCapture(ctx context.Context, cwd, explicit string) (commandCaptureTarget, error) {
	if container == nil || container.SessionSelection == nil {
		return commandCaptureTarget{}, fmt.Errorf("command session selection unavailable")
	}
	return container.SessionSelection.ResolveCommandSession(ctx, cwd, domain.ProviderKind(explicit))
}

func (container *Container) supervisedProvider(ctx context.Context, cwd string) (domain.ProviderKind, bool) {
	if container == nil || container.SessionSelection == nil {
		return "", false
	}
	return container.SessionSelection.SupervisedProvider(ctx, cwd)
}
