package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func runLiveObserver(ctx context.Context, cwd string, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("live-watch requires provider and registered session")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()
	run := func(phase string) func(context.Context) error {
		return func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "git-hook", phase, args[0], args[1])
			cmd.Dir = cwd
			return cmd.Run()
		}
	}
	// Capture and publication have independent bounds. The durable pending is
	// the retry checkpoint, so publication never repeats transcript parsing.
	return capture.WatchSession(ctx, cwd, domain.ProviderKind(args[0]), args[1], run("live-capture"), run("live-publish"))
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
	coord := capture.NewCaptureCoordinator(c.Save, c.Identity)
	// The observer supplies the 10-second cadence; retain growth/lock gates.
	if _, err := coord.RequestCapture(ctx, provider, cwd, session.Path, id, false, false); err != nil {
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
