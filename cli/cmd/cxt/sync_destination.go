package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
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
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	// Saved credentials are keyed by host, not scheme. Reject insecure remote
	// transport before either credential source can be used. Loopback remains
	// available for explicitly configured local development servers.
	ip := net.ParseIP(u.Hostname())
	loopback := strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback()
	if !strings.EqualFold(u.Scheme, "https") && !(strings.EqualFold(u.Scheme, "http") && loopback) {
		return ""
	}
	origin, err := url.Parse(base)
	if err == nil && strings.EqualFold(u.Scheme, origin.Scheme) && strings.EqualFold(u.Host, origin.Host) && strings.TrimRight(u.Path, "/") == strings.TrimRight(origin.Path, "/") && cfg.RemoteToken != "" {
		return cfg.RemoteToken
	}
	return authcfg.Token(u.Host)
}

func selectedPullApplication(store *storage.FileStore, remote *backendclient.BackendClient, git outbound.GitContext) func(context.Context, string) (outbound.SelectedPullReceipt, error) {
	service := app.NewSelectedPullService(store, remote, remote, gitctx.NewGitContextAdapter(), git, remote.SyncRemoteIdentity())
	return func(ctx context.Context, cwd string) (outbound.SelectedPullReceipt, error) {
		if err := service.InitializeSelection(ctx, app.SelectedPullInput{Cwd: cwd}); err != nil {
			return outbound.SelectedPullReceipt{}, err
		}
		return service.ApplyCurrent(ctx, app.SelectedPullInput{Cwd: cwd})
	}
}

func namedSyncDestination(cfg config, store *storage.FileStore, git outbound.GitContext) func(context.Context, string, string) (delivcli.SyncDestination, error) {
	return func(ctx context.Context, cwd, name string) (delivcli.SyncDestination, error) {
		var out delivcli.SyncDestination
		remotes, err := remotecfg.Load(cwd)
		if err != nil {
			return out, err
		}
		raw, exists := remotes[name]
		if !exists {
			return out, fmt.Errorf("remote %q is not configured", name)
		}
		base, err := remotecfg.APIBase(raw)
		if err != nil {
			return out, err
		}
		client := backendclient.NewBackendClient(func() string { return base }, func() string { return tokenForSyncDestination(cfg, base) }, cfg.Identity)
		repo, err := git.CurrentRepo(ctx, cwd)
		if err != nil {
			return out, err
		}
		connection, err := client.ResolveRepositoryConnection(ctx, raw)
		if err != nil {
			return out, fmt.Errorf("verify remote %q identity: %w", name, err)
		}
		if connection.RepoID != repo.ID {
			return out, fmt.Errorf("remote %q identifies a different repository; no local identity or refs changed", name)
		}
		client.SetChunkLocal(store)
		out.Sync = app.NewSyncRepoService(store, client, git, storage.NewSyncOutbox())
		out.ApplySelectedPull = selectedPullApplication(store, client, git)
		return out, nil
	}
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
		base, err := remotecfg.APIBase(canonical)
		if err != nil {
			return out, err
		}
		token := tokenForObservedDestination(cfg, base, configuredOrigin)
		client := backendclient.NewBackendClient(func() string { return base }, func() string { return token }, cfg.Identity)
		git := gitctx.NewGitContextAdapter()
		before, err := git.CurrentRepo(ctx, cwd)
		if err != nil {
			return out, err
		}
		connection, err := client.ResolveRepositoryConnection(ctx, canonical)
		if err != nil {
			return out, fmt.Errorf("repository identity could not be verified; remote was not saved: %w", err)
		}
		stable, err := delivcli.ValidatedRepositoryURL(canonical, connection)
		if err != nil {
			return out, err
		}
		repo := before
		repo.ID, repo.RemoteURL = connection.RepoID, stable
		// Connection preflight inspects existing document identities before any
		// registration writes. Constructing this local reader creates no files.
		store := storage.NewWorktreeFileStore(cfg.RepoRoot, cfg.GitDir, cfg.GitBranch, cfg.GitCommit)
		service := app.NewSyncRepoService(store, client, nil, nil)
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
