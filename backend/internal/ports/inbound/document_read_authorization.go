package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type documentReadAuthorizerKey struct{}
type documentSnapshotReadAuthorizerKey struct{}
type DocumentReadAuthorization func(context.Context, domain.ContentHash) (context.Context, error)

type DocumentReadAuthorizationBinding struct {
	authorize DocumentReadAuthorization
}

func WithDocumentReadAuthorizer(ctx context.Context, authorize DocumentReadAuthorization) context.Context {
	return context.WithValue(ctx, documentReadAuthorizerKey{}, &DocumentReadAuthorizationBinding{authorize: authorize})
}

func DocumentReadAuthorizer(ctx context.Context) DocumentReadAuthorization {
	if binding := DocumentReadAuthorizationBindingOf(ctx); binding != nil {
		return binding.authorize
	}
	return nil
}

func DocumentReadAuthorizationBindingOf(ctx context.Context) *DocumentReadAuthorizationBinding {
	binding, _ := ctx.Value(documentReadAuthorizerKey{}).(*DocumentReadAuthorizationBinding)
	return binding
}

func WithDocumentSnapshotReadAuthorizer(ctx context.Context, authorize DocumentReadAuthorization) context.Context {
	return context.WithValue(ctx, documentSnapshotReadAuthorizerKey{}, authorize)
}

func DocumentSnapshotReadAuthorizer(ctx context.Context) DocumentReadAuthorization {
	authorize, _ := ctx.Value(documentSnapshotReadAuthorizerKey{}).(DocumentReadAuthorization)
	return authorize
}
