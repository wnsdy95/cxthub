package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// GitReadFence is a local policy check executed inside the publication transaction.
// It must not perform provider I/O or acquire an identity write transaction.
type GitReadFence = outbound.GitReadFence

type GitSourceAuthorizer = outbound.GitSourceAuthorizer

type GitObservationAuthorizer interface {
	AuthorizeGitObservation(context.Context, domain.ContentHash) (*domain.GitProviderObservation, GitReadFence, error)
}

func authorizeGitObservation(ctx context.Context, policy GitSourceAuthorizer, repo domain.ContentHash) (*domain.GitProviderObservation, GitReadFence, error) {
	if qualified, ok := policy.(GitObservationAuthorizer); ok {
		identity, fence, err := qualified.AuthorizeGitObservation(ctx, repo)
		if err != nil {
			return nil, nil, err
		}
		if fence == nil || (identity != nil && identity.Validate() != nil) {
			return nil, nil, domain.ErrIntegrity
		}
		if identity != nil {
			captured := *identity
			identity = &captured
		}
		return identity, fence, nil
	}
	fence, err := authorizeGitRead(ctx, policy, repo)
	return nil, fence, err
}

func authorizeGitRead(ctx context.Context, policy GitSourceAuthorizer, repo domain.ContentHash) (GitReadFence, error) {
	if policy == nil {
		return func(context.Context) error { return nil }, nil
	}
	fence, err := policy.AuthorizeGitRead(ctx, repo)
	if err == nil && fence == nil {
		return nil, domain.ErrIntegrity
	}
	return fence, err
}

func (connections *GitHubConnections) AuthorizeGitRead(ctx context.Context, repoID domain.ContentHash) (GitReadFence, error) {
	return connections.worker().AuthorizeGitRead(ctx, repoID)
}

func (connections *GitHubConnections) AuthorizeGitObservation(ctx context.Context, repoID domain.ContentHash) (*domain.GitProviderObservation, GitReadFence, error) {
	return connections.worker().AuthorizeGitObservation(ctx, repoID)
}

func (worker *GitHubWorker) AuthorizeGitRead(ctx context.Context, repoID domain.ContentHash) (GitReadFence, error) {
	_, fence, err := worker.AuthorizeGitObservation(ctx, repoID)
	return fence, err
}

func (worker *GitHubWorker) AuthorizeGitObservation(ctx context.Context, repoID domain.ContentHash) (*domain.GitProviderObservation, GitReadFence, error) {
	repo, err := worker.metadata.GetRepo(ctx, repoID)
	if err != nil {
		return nil, nil, err
	}
	local, err := worker.repositories.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return nil, nil, err
	}
	connection, err := worker.store.GetGitHubConnection(ctx, local.OwnerNamespaceID)
	if errors.Is(err, domain.ErrNotFound) {
		// Installing a connection while an operator-token request is in flight also
		// changes the source contract; retry through the new explicit binding.
		return nil, func(tx context.Context) error {
			current, lookupError := worker.metadata.GetRepo(tx, repoID)
			if lookupError != nil {
				return lookupError
			}
			if current.RepositoryID != repo.RepositoryID {
				return domain.ErrConflict
			}
			currentLocal, lookupError := worker.repositories.GetRepository(tx, local.ID)
			if lookupError != nil {
				return lookupError
			}
			if currentLocal.OwnerNamespaceID != local.OwnerNamespaceID {
				return domain.ErrConflict
			}
			_, lookupError = worker.store.GetGitHubConnection(tx, local.OwnerNamespaceID)
			if errors.Is(lookupError, domain.ErrNotFound) {
				return nil
			}
			if lookupError != nil {
				return lookupError
			}
			return domain.ErrConflict
		}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, binding := range connection.Bindings {
		if binding.ContextRepoID != repoID {
			continue
		}
		if _, err := worker.binding(ctx, connection, binding); err != nil {
			return nil, nil, err
		}
		identity := domain.GitProviderObservation{InstallationID: connection.Installation.ID, RepositoryID: binding.ExternalID}
		if err := identity.Validate(); err != nil {
			return nil, nil, err
		}
		return &identity, func(tx context.Context) error { _, err := worker.binding(tx, connection, binding); return err }, nil
	}
	return nil, nil, domain.ErrForbidden
}
