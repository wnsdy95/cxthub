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

func authorizeGitRead(ctx context.Context, policy GitSourceAuthorizer, repo domain.ContentHash) (GitReadFence, error) {
	if policy == nil {
		return func(context.Context) error { return nil }, nil
	}
	return policy.AuthorizeGitRead(ctx, repo)
}

func (g *GitHubConnections) AuthorizeGitRead(ctx context.Context, repoID domain.ContentHash) (GitReadFence, error) {
	repo, err := g.core.meta.GetRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	local, err := g.id.repositories.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return nil, err
	}
	c, err := g.store.GetGitHubConnection(ctx, local.OwnerNamespaceID)
	if errors.Is(err, domain.ErrNotFound) {
		// Installing a connection while an operator-token request is in flight also
		// changes the source contract; retry through the new explicit binding.
		return func(tx context.Context) error {
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
		return nil, err
	}
	for _, b := range c.Bindings {
		if b.ContextRepoID != repoID {
			continue
		}
		if _, err := g.binding(ctx, c, b); err != nil {
			return nil, err
		}
		return func(tx context.Context) error { _, err := g.binding(tx, c, b); return err }, nil
	}
	return nil, domain.ErrForbidden
}
