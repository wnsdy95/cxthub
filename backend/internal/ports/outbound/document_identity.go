package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Called under the existing repository write transaction. This is persistence,
// not authorization; the application owns peer, policy and release admission.
type RepositoryDocumentIdentityStore interface {
	RequireDocumentIdentity(context.Context, domain.ContentHash, domain.DocumentIdentity) error
}

// Compatibility accompanies adapter-owned read snapshots and cache transactions.
// It is not authentication, a verification proof or a publication permission.
type documentIdentityCompatibilityKey struct{}
type documentIdentityCompatibility struct{ peer, binary []domain.DocumentIdentity }

func WithDocumentIdentityCompatibility(ctx context.Context, peer, binary []domain.DocumentIdentity) context.Context {
	return context.WithValue(ctx, documentIdentityCompatibilityKey{}, documentIdentityCompatibility{append([]domain.DocumentIdentity(nil), peer...), append([]domain.DocumentIdentity(nil), binary...)})
}
func CheckDocumentIdentityCompatibility(ctx context.Context, required domain.DocumentIdentity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := required.Validate(); err != nil {
		return err
	}
	declaration, _ := ctx.Value(documentIdentityCompatibilityKey{}).(documentIdentityCompatibility)
	for _, ids := range [][]domain.DocumentIdentity{declaration.peer, declaration.binary} {
		found := required == domain.DocumentIdentityLegacy
		for _, id := range ids {
			if err := id.Validate(); err != nil {
				return err
			}
			if id == required {
				found = true
			}
		}
		if !found {
			return domain.ErrDocumentIdentityUpgradeRequired
		}
	}
	return nil
}
