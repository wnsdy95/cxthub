package main

import (
	"context"
	"fmt"
	"net/url"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func verifyEndpointRepository(ctx context.Context, client *backendclient.BackendClient, binding remotecfg.EndpointBinding) (remotecfg.EndpointBinding, error) {
	connection, err := client.ResolveRepositoryConnection(ctx, binding.RemoteURL)
	if err != nil {
		return binding, fmt.Errorf("destination repository connection verification failed")
	}
	if connection.RemoteURL != binding.RemoteURL || connection.RepoID != binding.RepoID || connection.RepositoryID == "" || binding.RepositoryID != "" && connection.RepositoryID != binding.RepositoryID {
		return binding, fmt.Errorf("%w: destination does not match the existing repository identity", remotecfg.ErrEndpointBindingMismatch)
	}
	repo, err := client.Repository(ctx, binding.RepoID)
	if err != nil {
		return binding, fmt.Errorf("destination existing repository verification failed")
	}
	if repo.ID != binding.RepoID || repo.RemoteURL != binding.RemoteURL || repo.RepositoryID != connection.RepositoryID {
		return binding, fmt.Errorf("%w: destination existing repository binding does not match", remotecfg.ErrEndpointBindingMismatch)
	}
	binding.Version, binding.RepositoryID = 1, connection.RepositoryID
	return binding, binding.Validate()
}

func resolveEndpointConnection(ctx context.Context, client *backendclient.BackendClient, binding remotecfg.EndpointBinding) (domain.RepositoryConnection, error) {
	if binding.Version == 0 {
		return client.ResolveRepositoryConnection(ctx, binding.RemoteURL)
	}
	if _, err := verifyEndpointRepository(ctx, client, binding); err != nil {
		return domain.RepositoryConnection{}, err
	}
	return domain.RepositoryConnection{RemoteURL: binding.RemoteURL, RepoID: binding.RepoID, RepositoryID: binding.RepositoryID}, nil
}

func setRemoteEndpoint(cfg config) func(context.Context, remotecfg.Observation, string, string, bool) error {
	return func(ctx context.Context, observed remotecfg.Observation, name, raw string, migratePending bool) error {
		endpoint, err := remotecfg.CanonicalEndpoint(raw)
		if err != nil {
			return err
		}
		remotes, err := observed.Remotes()
		if err != nil {
			return err
		}
		if remotes[name] == "" {
			return fmt.Errorf("remote is not configured")
		}
		binding, err := observed.ResolveEndpoint(remotes[name])
		if err != nil {
			return err
		}
		binding.Endpoint = endpoint
		server, _ := url.Parse(endpoint)
		token := authcfg.Token(server.Host)
		if token == "" {
			return fmt.Errorf("destination login required: cxt login --server %s://%s; config unchanged", server.Scheme, server.Host)
		}
		client := backendclient.NewBackendClient(func() string { return endpoint }, func() string { return token }, domain.TeamIdentity{})
		if err := client.ConfirmAuthenticatedConnection(ctx); err != nil {
			return err
		}
		binding, err = verifyEndpointRepository(ctx, client, binding)
		if err != nil {
			return err
		}
		identity, err := observed.CaptureIdentity()
		if err != nil {
			return err
		}
		rootConfirmed := false
		if remotes["origin"] == binding.RemoteURL && identity == domain.DocumentIdentityRootV1 {
			if err := client.ConfirmRootCapture(ctx, binding.RepoID); err != nil {
				return fmt.Errorf("destination root capture capability not confirmed; config unchanged")
			}
			rootConfirmed = true
		}
		return remotecfg.SetEndpointWithPending(ctx, observed, name, binding, rootConfirmed, migratePending)
	}
}

func configuredTransport(cfg config) (string, string, error) {
	observed, err := remotecfg.Observe(context.Background(), cfg.RepoRoot)
	if err != nil {
		return "", "", err
	}
	binding, err := observed.OriginEndpoint(cfg.RemoteEndpoint)
	if err != nil {
		return "", "", err
	}
	return binding.Endpoint, tokenForObservedDestination(cfg, binding.Endpoint, binding.RemoteURL), nil
}
