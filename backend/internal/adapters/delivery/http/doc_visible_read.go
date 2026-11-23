package http

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) docVisibleReadAuthorizer(r *http.Request, repo domain.ContentHash) inbound.DocumentReadAuthorization {
	token := s.requestToken(r)
	return func(ctx context.Context, requestedRepo domain.ContentHash) (context.Context, error) {
		ctx = inbound.WithRepositoryActor(context.WithValue(ctx, userCtxKey, struct{}{}), "")
		if requestedRepo != repo {
			return ctx, &httpAccessError{status: http.StatusForbidden, code: "forbidden", cause: domain.ErrForbidden,
				message: "document read repository does not match request"}
		}
		if err := ctx.Err(); err != nil {
			return ctx, err
		}
		if token != "" {
			if user, err := s.id.ResolveUser(ctx, token); err == nil {
				ctx = inbound.WithRepositoryActor(context.WithValue(ctx, userCtxKey, user), user.ID)
			}
		}
		return ctx, s.checkRepoRole(ctx, repo, domain.RoleViewer)
	}
}
