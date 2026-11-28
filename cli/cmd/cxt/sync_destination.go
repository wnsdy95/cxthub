package main

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	delivcli "github.com/wnsdy95/cxthub-cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/app"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

// Only the configured origin may inherit CXT_TOKEN. Other servers use their
// own login credential, so adding a named remote cannot leak origin credentials.
func tokenForSyncDestination(cfg config, target string) string {
	origin, _ := remotecfg.Origin(cfg.RepoRoot)
	return tokenForObservedDestination(cfg, target, origin)
}

func tokenForObservedDestination(cfg config, target, configuredOrigin string) string {
	base := cfg.RemoteEndpoint
	if configuredOrigin != "" {
		if resolved, err := remotecfg.APIBase(configuredOrigin); err == nil {
			base = resolved
		}
	}
	return authcfg.TokenForDestination(target, base, cfg.RemoteToken)
}

// prepareRemoteConnection freezes endpoint, credential and Git evidence once.
// Both resolution and registration use this client; neither reads live config.
func prepareRemoteConnection(cfg config) func(context.Context, string, string, string) (delivcli.PreparedRemoteConnection, error) {
	return func(ctx context.Context, cwd, raw, configuredOrigin string) (delivcli.PreparedRemoteConnection, error) {
		var out delivcli.PreparedRemoteConnection
		canonical, err := remotecfg.CanonicalURL(raw)
		if err != nil {
			return out, err
		}
		binding, err := remotecfg.ResolveEndpoint(ctx, cwd, canonical)
		if err != nil {
			return out, err
		}
		base := binding.Endpoint
		token := tokenForObservedDestination(cfg, base, configuredOrigin)
		client := backendclient.NewBackendClient(func() string { return base }, func() string { return token }, cfg.Identity)
		git := gitctx.NewGitContextAdapter()
		before, err := git.CurrentRepo(ctx, cwd)
		if err != nil {
			return out, err
		}
		connection, err := resolveEndpointConnection(ctx, client, binding)
		if err != nil {
			return out, fmt.Errorf("repository identity could not be verified; remote was not saved: %w", err)
		}
		stable, err := delivcli.ValidatedRepositoryURL(canonical, connection)
		if err != nil {
			return out, err
		}
		repo := before
		repo.ID, repo.RemoteURL = connection.RepoID, stable
		service := app.NewRepositoryConnectService(client, nil)
		out.URL = stable
		out.Connect = func(ctx context.Context) (inbound.ConnectOutput, error) { return service.ConnectRepository(ctx, repo) }
		out.ValidateLocal = func(ctx context.Context) error {
			current, err := git.CurrentRepo(ctx, cwd)
			if err != nil {
				return err
			}
			if current != before {
				return domain.ErrSelectionChanged
			}
			return nil
		}
		return out, nil
	}
}
