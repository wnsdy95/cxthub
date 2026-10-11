package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// GitReadFence is a local policy check executed inside the publication transaction.
// It must not perform provider I/O or acquire an identity write transaction.
type GitReadFence func(context.Context) error

type GitSourceAuthorizer interface {
	AuthorizeGitRead(context.Context, domain.ContentHash) (GitReadFence, error)
}

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
	return policy.AuthorizeGitRead(ctx, repo)
}

func (g *GitHubConnections) AuthorizeGitRead(ctx context.Context, repoID domain.ContentHash) (GitReadFence, error) {
	_, fence, err := g.AuthorizeGitObservation(ctx, repoID)
	return fence, err
}

func (g *GitHubConnections) AuthorizeGitObservation(ctx context.Context, repoID domain.ContentHash) (*domain.GitProviderObservation, GitReadFence, error) {
	repo, err := g.core.meta.GetRepo(ctx, repoID)
	if err != nil {
		return nil, nil, err
	}
	local, err := g.id.repositories.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return nil, nil, err
	}
	c, err := g.store.GetGitHubConnection(ctx, local.OwnerNamespaceID)
	if errors.Is(err, domain.ErrNotFound) {
		// Installing a connection while an operator-token request is in flight also
		// changes the source contract; retry through the new explicit binding.
		return nil, func(tx context.Context) error {
			current, e := g.core.meta.GetRepo(tx, repoID)
			if e != nil {
				return e
			}
			if current.RepositoryID != repo.RepositoryID {
				return domain.ErrConflict
			}
			now, e := g.id.repositories.GetRepository(tx, local.ID)
			if e != nil {
				return e
			}
			if now.OwnerNamespaceID != local.OwnerNamespaceID {
				return domain.ErrConflict
			}
			_, e = g.store.GetGitHubConnection(tx, local.OwnerNamespaceID)
			if errors.Is(e, domain.ErrNotFound) {
				return nil
			}
			if e != nil {
				return e
			}
			return domain.ErrConflict
		}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, b := range c.Bindings {
		if b.ContextRepoID != repoID {
			continue
		}
		if _, err := g.binding(ctx, c, b); err != nil {
			return nil, nil, err
		}
		identity := domain.GitProviderObservation{InstallationID: c.Installation.ID, RepositoryID: b.ExternalID}
		if err := identity.Validate(); err != nil {
			return nil, nil, err
		}
		return &identity, func(tx context.Context) error { _, err := g.binding(tx, c, b); return err }, nil
	}
	return nil, nil, domain.ErrForbidden
}
