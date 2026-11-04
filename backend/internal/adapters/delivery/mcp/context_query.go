package mcp

import (
	"context"
	"fmt"
	"net/http"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type contextQueryAuthorizationKey struct{}

func (server *Server) SetContextQuery(query inbound.ContextQuery, profile inbound.ContextQueryProfile) error {
	if profile != inbound.ContextQueryCompatibility && profile != inbound.ContextQueryPinnedSnapshots {
		return fmt.Errorf("%w: explicit context query profile is required", domain.ErrValidation)
	}
	if query == nil {
		return fmt.Errorf("%w: context query is required", domain.ErrValidation)
	}
	switch value := reflect.ValueOf(query); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("%w: context query is required", domain.ErrValidation)
		}
	}
	server.contextQueries = query
	server.contextQueryProfile = profile
	return nil
}

func (server *Server) queryContext(ctx context.Context, repo domain.ContentHash, selection domain.ContextSelection) (domain.ContextQueryView, error) {
	if server.contextQueries == nil {
		return domain.ContextQueryView{}, fmt.Errorf("%w: context query is not configured", domain.ErrAgentHistoryUnavailable)
	}
	if authorize, ok := ctx.Value(contextQueryAuthorizationKey{}).(inbound.DocumentReadAuthorization); ok {
		ctx = inbound.WithDocumentReadAuthorizer(ctx, authorize)
	}
	return server.contextQueries.QueryContext(ctx, repo, selection)
}

func (server *Server) protocolReadAuthorizer(request *http.Request, user domain.User, pinned bool) inbound.DocumentReadAuthorization {
	token := bearerToken(request)
	var snapshotAuthenticate func(context.Context) (domain.User, error)
	if identity, ok := server.identity.(inbound.SnapshotReadIdentity); ok && pinned {
		snapshotAuthenticate = func(bound context.Context) (domain.User, error) {
			return identity.ResolveMCPUserForSnapshot(bound, token, server.resource)
		}
	}
	return server.memoryReadAuthorizer(user, func(bound context.Context) (domain.User, error) {
		if token == "" {
			return domain.User{}, domain.ErrUnauthorized
		}
		return server.identity.ResolveMCPUser(bound, token, server.resource)
	}, snapshotAuthenticate)
}
