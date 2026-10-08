package outbound

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"testing"
)

func TestP6StoreCompatibilityRequiresBothCopiedDeclarations(t *testing.T) {
	root := domain.DocumentIdentityRootV1
	if err := CheckDocumentIdentityCompatibility(context.Background(), root); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("missing context bypass", err)
	}
	peer := []domain.DocumentIdentity{root}
	binary := []domain.DocumentIdentity{root}
	ctx := WithDocumentIdentityCompatibility(context.Background(), peer, binary)
	peer[0] = "future"
	binary[0] = "future"
	if err := CheckDocumentIdentityCompatibility(ctx, root); err != nil {
		t.Fatal("declarations aliased", err)
	}
	for _, ctx := range []context.Context{WithDocumentIdentityCompatibility(context.Background(), nil, []domain.DocumentIdentity{root}), WithDocumentIdentityCompatibility(context.Background(), []domain.DocumentIdentity{root}, nil)} {
		if err := CheckDocumentIdentityCompatibility(ctx, root); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
			t.Fatal("single declaration bypass", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := CheckDocumentIdentityCompatibility(canceled, root); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	if err := CheckDocumentIdentityCompatibility(context.Background(), domain.DocumentIdentityLegacy); err != nil {
		t.Fatal("legacy changed", err)
	}
}
