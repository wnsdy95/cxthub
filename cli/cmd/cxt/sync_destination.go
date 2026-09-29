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
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Only the configured origin may inherit CXT_TOKEN. Other servers use their
// own login credential, so adding a named remote cannot leak origin credentials.
func tokenForSyncDestination(cfg config, target string) string {
	base := cfg.RemoteEndpoint
	if origin, ok := remotecfg.Origin(cfg.RepoRoot); ok {
		if resolved, err := remotecfg.APIBase(origin); err == nil {
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
