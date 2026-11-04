package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

func runLiveObserver(ctx context.Context, c *Container, cwd string, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("live-watch requires provider and registered session")
	}
	if c == nil || c.ObserveLiveSession == nil {
		return fmt.Errorf("live session observer unavailable")
	}
	return c.ObserveLiveSession(ctx, cwd, domain.ProviderKind(args[0]), args[1])
}

func runLiveCapture(ctx context.Context, c *Container, cwd string, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("live-capture requires provider and registered session")
	}
	provider, id := domain.ProviderKind(args[0]), args[1]
	session, ok := capture.RegisteredSession(cwd, provider, id)
	if !ok {
		return nil
	}
	if c == nil || c.RequestCapture == nil {
		return fmt.Errorf("live capture coordinator unavailable")
	}
	// The observer supplies the 10-second cadence; retain growth/lock gates.
	if _, err := c.RequestCapture(ctx, provider, cwd, session.Path, id, false, false); err != nil {
		return err
	}
	return nil
}

func runLivePublish(ctx context.Context, c *Container, cwd string, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("live-publish requires provider and registered session")
	}
	provider, id := domain.ProviderKind(args[0]), args[1]
	session, ok := capture.RegisteredSession(cwd, provider, id)
	if !ok {
		return nil
	}
	root := cxtRepoRoot(ctx, cwd)
	if _, ok := remotecfg.Origin(root); !ok && os.Getenv("CXT_REMOTE") == "" {
		return nil
	}
	nativeID := session.NativeID
	if nativeID == "" {
		nativeID = id
	}
	_, err := c.Sync.SyncPendings(ctx, inbound.SyncInput{Cwd: cwd, PendingSessionID: nativeID}, nil)
	return err
}
