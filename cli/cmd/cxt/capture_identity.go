package main

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func setCaptureIdentity(cfg config) func(context.Context, string, domain.DocumentIdentity) error {
	return func(ctx context.Context, cwd string, identity domain.DocumentIdentity) error {
		if err := identity.Validate(); err != nil {
			return err
		}
		observed, err := remotecfg.Observe(ctx, cwd)
		if err != nil {
			return err
		}
		if identity == domain.DocumentIdentityLegacy {
			return remotecfg.SetCaptureDocumentIdentity(ctx, observed, identity)
		}
		remotes, err := observed.Remotes()
		if err != nil {
			return err
		}
		origin := remotes["origin"]
		if origin == "" {
			return fmt.Errorf("capture.identity requires a configured origin")
		}
		endpoint, err := remotecfg.APIBase(origin)
		if err != nil {
			return err
		}
		token := tokenForObservedDestination(cfg, endpoint, origin)
		remote := backendclient.NewBackendClient(func() string { return endpoint }, func() string { return token }, cfg.Identity)
		if err := remote.ConfirmRootCapture(ctx, remotecfg.RepoIDFor(origin)); err != nil {
			return fmt.Errorf("root capture capability was not confirmed; preference unchanged: %w", err)
		}
		return remotecfg.SetCaptureDocumentIdentity(ctx, observed, identity)
	}
}
