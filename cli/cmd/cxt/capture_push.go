package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"

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

// The caller has checked raw against its durable destination hash. Do not load
// origin here: endpoint, credential and repository identity belong to this
// operation, including all of its requests and the bounded append retry.
func prepareCapturePush(cfg config, store *storage.FileStore) func(context.Context, string, delivcli.CapturePushDestination, string) (inbound.SyncRepo, error) {
	return func(ctx context.Context, cwd string, destination delivcli.CapturePushDestination, repoID string) (inbound.SyncRepo, error) {
		if err := domain.ValidateContentHash(repoID); err != nil {
			return nil, err
		}
		var base, canonical, token string
		if destination.APIOnly {
			// CXT_REMOTE is an API base, never a repository URL. Preserve its
			// path and prove the exact existing repo with GET before any write.
			u, err := url.Parse(destination.URL)
			if err != nil || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
				return nil, fmt.Errorf("invalid bound API destination")
			}
			base = strings.TrimRight(destination.URL, "/")
			observed := cfg
			observed.RemoteEndpoint = base
			token = tokenForObservedDestination(observed, base, "")
		} else {
			var err error
			canonical, err = remotecfg.CanonicalURL(destination.URL)
			if err != nil {
				return nil, err
			}
			base, err = remotecfg.APIBase(canonical)
			if err != nil {
				return nil, err
			}
			token = tokenForObservedDestination(cfg, base, destination.URL)
		}
		client := backendclient.NewBackendClient(func() string { return base }, func() string { return token }, cfg.Identity)
		git := gitctx.NewGitContextAdapter()
		repo, err := git.CurrentRepo(ctx, cwd)
		if err != nil {
			return nil, err
		}
		if destination.APIOnly {
			if repo.ID != repoID {
				return nil, domain.ErrHashMismatch
			}
			if _, err := client.Repository(ctx, repoID); err != nil {
				return nil, err
			}
		} else {
			connection, err := client.ResolveRepositoryConnection(ctx, canonical)
			if err != nil {
				return nil, err
			}
			stable, err := delivcli.ValidatedRepositoryURL(canonical, connection)
			if err != nil {
				return nil, err
			}
			if connection.RepoID != repoID {
				return nil, domain.ErrHashMismatch
			}
			repo.ID, repo.RemoteURL = repoID, stable
		}
		client.SetChunkLocal(store)
		client.SetMetadataCheckpointStore(store)
		client.SetCatalogCacheStore(store)
		bound := capturePushGit{GitContext: git, cwd: cwd, repo: repo}
		return app.NewSyncRepoService(store, client, bound, storage.NewSyncOutbox()), nil
	}
}

// Keep normal code-branch queries, but never reinterpret repository identity
// from mutable Git/CXT configuration after the destination has been verified.
type capturePushGit struct {
	outbound.GitContext
	cwd  string
	repo domain.Repo
}

func (g capturePushGit) CurrentRepo(ctx context.Context, cwd string) (domain.Repo, error) {
	if err := ctx.Err(); err != nil {
		return domain.Repo{}, err
	}
	if cwd != g.cwd {
		return domain.Repo{}, domain.ErrSelectionChanged
	}
	return g.repo, nil
}
