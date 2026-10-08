package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type documentIdentitiesKey struct{}

// A declaration is compatibility metadata only. It neither assigns an actor
// nor makes root bytes, ownership or publication policy valid.
func WithDocumentIdentities(ctx context.Context, ids []domain.DocumentIdentity) context.Context {
	return context.WithValue(ctx, documentIdentitiesKey{}, append([]domain.DocumentIdentity(nil), ids...))
}
func DocumentIdentities(ctx context.Context) []domain.DocumentIdentity {
	ids, _ := ctx.Value(documentIdentitiesKey{}).([]domain.DocumentIdentity)
	return append([]domain.DocumentIdentity(nil), ids...)
}

type DocumentIdentityCapabilities interface {
	DocumentIdentitiesSupported() []domain.DocumentIdentity
	RootPublicationEnabled() bool
}
