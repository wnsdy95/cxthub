package cli

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// SyncDestination is resolved before replay, object upload, or pointer changes.
// A named remote never changes the local repository's immutable identity.
type SyncDestination struct {
	Sync              inbound.SyncRepo
	ApplySelectedPull func(context.Context, string) (outbound.SelectedPullReceipt, error)
}

func syncDestination(ctx context.Context, c *Container, cwd string, p parsedCommand) (*Container, string, string, error) {
	if len(p.positionals) == 0 {
		if err := requireRemote(cwd); err != nil {
			return nil, "", "", err
		}
		return c, "origin", "", nil
	}
	name := p.positionals[0]
	if c.ResolveSyncDestination == nil {
		return nil, "", "", fmt.Errorf("named remote resolution unavailable")
	}
	destination, err := c.ResolveSyncDestination(ctx, cwd, name)
	if err != nil {
		return nil, "", "", err
	}
	if destination.Sync == nil {
		return nil, "", "", fmt.Errorf("remote %q has no sync client", name)
	}
	selected := *c
	selected.Sync, selected.ApplySelectedPull = destination.Sync, destination.ApplySelectedPull
	// The background uploader is scoped to configured origin. Named destinations
	// complete in the foreground, so no task can accidentally upload to origin.
	selected.WakeHistoricalSync = nil
	ref := ""
	if len(p.positionals) == 2 {
		ref = p.positionals[1]
	}
	return &selected, name, ref, nil
}
