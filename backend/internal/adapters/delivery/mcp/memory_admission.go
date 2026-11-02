package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

const memoryRequestLimit = 2 * time.Minute

type memoryRepositoryKey struct{}

func (server *Server) memoryReadAuthorizer(user domain.User, authenticate func(context.Context) (domain.User, error), snapshotAuthenticate ...func(context.Context) (domain.User, error)) inbound.DocumentReadAuthorization {
	return func(ctx context.Context, repoID domain.ContentHash) (context.Context, error) {
		if err := ctx.Err(); err != nil {
			return ctx, err
		}
		current := user
		if authenticate != nil {
			var err error
			current, err = authenticate(ctx)
			if err != nil {
				return ctx, err
			}
		}
		if user.ID == "" || current.ID != user.ID {
			return ctx, domain.ErrUnauthorized
		}
		bound := inbound.WithRepositoryActor(ctx, current.ID)
		lookup := inbound.WithDocumentReadAuthorizer(bound, nil)
		repos, err := server.context.ListRepos(lookup, "default")
		if err != nil {
			return ctx, err
		}
		for _, repo := range repos {
			if repo.ID != repoID {
				continue
			}
			if identity, ok := server.identity.(inbound.SnapshotReadIdentity); ok && len(snapshotAuthenticate) == 1 && snapshotAuthenticate[0] != nil {
				permit, err := identity.AdmitRepositoryRead(lookup, repo.RepositoryID, current.ID)
				if err != nil {
					return ctx, err
				}
				snapshot := server.memorySnapshotAuthorizer(current, repo, permit, snapshotAuthenticate[0])
				bound = inbound.WithDocumentSnapshotReadAuthorizer(bound, snapshot)
				return context.WithValue(bound, memoryRepositoryKey{}, repo), nil
			}
			allowed, err := server.canReadRepository(lookup, current, repo)
			if err != nil {
				return ctx, err
			}
			if !allowed {
				return ctx, domain.ErrForbidden
			}
			return context.WithValue(bound, memoryRepositoryKey{}, repo), nil
		}
		return ctx, domain.ErrNotFound
	}
}

func (server *Server) memorySnapshotAuthorizer(user domain.User, repo domain.Repo, permit inbound.RepositoryReadPermit, authenticate func(context.Context) (domain.User, error)) inbound.DocumentReadAuthorization {
	return func(ctx context.Context, requested domain.ContentHash) (context.Context, error) {
		ctx = inbound.WithRepositoryActor(ctx, "")
		if requested != repo.ID {
			return ctx, domain.ErrForbidden
		}
		current, err := authenticate(ctx)
		if err != nil {
			return ctx, err
		}
		if current.ID == "" || current.ID != user.ID {
			return ctx, domain.ErrUnauthorized
		}
		lookup := inbound.WithDocumentReadAuthorizer(ctx, nil)
		reader, ok := server.context.(interface {
			GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
		})
		if !ok {
			return ctx, domain.ErrCatalogUnsupported
		}
		candidate, err := reader.GetRepo(lookup, requested)
		if err != nil {
			return ctx, err
		}
		if candidate.ID != requested || candidate.RepositoryID != repo.RepositoryID {
			return ctx, domain.ErrForbidden
		}
		if err := permit.AuthorizeSnapshot(ctx, candidate.RepositoryID, current.ID); err != nil {
			return ctx, err
		}
		bound := inbound.WithRepositoryActor(ctx, current.ID)
		return context.WithValue(bound, memoryRepositoryKey{}, candidate), nil
	}
}

func (server *Server) memoryLoad(ctx context.Context, user domain.User, args toolArgs) (string, error) {
	if args.ReadVersion != nil {
		if server.preparedMemory == nil {
			return "", domain.ErrCatalogUnsupported
		}
		return server.memoryLoadWithScope(ctx, user, args, server.preparedMemory)
	}
	if memorySnapshotOnly(args) {
		if server.memorySnapshots == nil {
			return "", errMemorySnapshotsUnavailable
		}
		return server.memoryLoadWithScope(ctx, user, args, server.memorySnapshots)
	}
	var scope inbound.MemoryReadScope = server.branchMemory
	if args.Mode == "effective" {
		scope = server.effectiveMemory
	}
	if scope == nil {
		return "", fmt.Errorf("live memory read admission unavailable")
	}
	return server.memoryLoadWithScope(ctx, user, args, scope)
}

func (server *Server) memoryLoadWithScope(ctx context.Context, user domain.User, args toolArgs, scope inbound.MemoryReadScope) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, memoryRequestLimit)
	defer cancel()
	repo, err := server.resolveRepository(ctx, user, args.Repository)
	if err != nil {
		return "", err
	}
	if inbound.DocumentReadAuthorizer(ctx) == nil {
		ctx = inbound.WithDocumentReadAuthorizer(ctx, server.memoryReadAuthorizer(user, nil))
	}
	var result string
	err = scope.WithMemoryReadResources(ctx, repo.ID, func(bound context.Context) error {
		current, ok := bound.Value(memoryRepositoryKey{}).(domain.Repo)
		if !ok || current.ID != repo.ID {
			return domain.ErrForbidden
		}
		if err := bound.Err(); err != nil {
			return err
		}
		var err error
		result, err = server.memoryPage(bound, current, args)
		if err != nil {
			return err
		}
		return bound.Err()
	})
	if err != nil {
		return "", err
	}
	return result, nil
}
