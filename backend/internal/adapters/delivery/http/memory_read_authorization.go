package http

import (
	"context"
	"errors"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (server *Server) memoryReadAuthorizer(request *http.Request, repoID domain.ContentHash) inbound.DocumentReadAuthorization {
	identity, ok := server.id.(inbound.SnapshotReadIdentity)
	if !ok {
		return server.docVisibleReadAuthorizer(request, repoID)
	}
	token := server.requestToken(request)
	return func(ctx context.Context, requested domain.ContentHash) (context.Context, error) {
		ctx = inbound.WithRepositoryActor(context.WithValue(ctx, userCtxKey, struct{}{}), "")
		if err := ctx.Err(); err != nil {
			return ctx, err
		}
		if requested != repoID {
			return ctx, domain.ErrForbidden
		}
		var user domain.User
		if token != "" {
			if resolved, err := server.id.ResolveUser(ctx, token); err == nil {
				user = resolved
			}
		}
		repo, err := server.b.GetRepo(ctx, repoID)
		if err != nil {
			return ctx, err
		}
		if repo.RepositoryID == "" {
			return ctx, domain.ErrForbidden
		}
		permit, err := identity.AdmitRepositoryRead(ctx, repo.RepositoryID, user.ID)
		if err != nil {
			return ctx, err
		}
		if user.ID != "" {
			ctx = inbound.WithRepositoryActor(context.WithValue(ctx, userCtxKey, user), user.ID)
		}
		snapshot := func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
			bound = inbound.WithRepositoryActor(context.WithValue(bound, userCtxKey, struct{}{}), "")
			if requested != repoID {
				return bound, domain.ErrForbidden
			}
			current := domain.User{}
			if user.ID != "" {
				var err error
				current, err = identity.ResolveUserForSnapshot(bound, token)
				if err != nil && !errors.Is(err, domain.ErrUnauthorized) {
					return bound, err
				}
				if err != nil {
					current = domain.User{}
				}
				if current.ID != "" && current.ID != user.ID {
					return bound, domain.ErrUnauthorized
				}
			}
			currentRepo, err := server.b.GetRepo(bound, requested)
			if err != nil {
				return bound, err
			}
			if currentRepo.RepositoryID != repo.RepositoryID {
				return bound, domain.ErrForbidden
			}
			if err := permit.AuthorizeSnapshot(bound, currentRepo.RepositoryID, current.ID); err != nil {
				return bound, err
			}
			if current.ID != "" {
				bound = inbound.WithRepositoryActor(context.WithValue(bound, userCtxKey, current), current.ID)
			}
			return bound, nil
		}
		return inbound.WithDocumentSnapshotReadAuthorizer(ctx, snapshot), nil
	}
}
