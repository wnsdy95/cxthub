package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// The caller reads and retains this requirement in its final publication
// boundary. Admission policy may later be disabled without abandoning accepted
// jobs; current binary compatibility and repository opt-in are still required.
func checkDocJobPublicationPolicy(ctx context.Context, repo domain.Repo, identity domain.DocumentIdentity) error {
	if err := outbound.CheckDocumentIdentityCompatibility(ctx, repo.RequiredDocIdentity); err != nil {
		return err
	}
	if identity == domain.DocumentIdentityRootV1 && repo.RequiredDocIdentity != identity {
		return domain.ErrRootPublicationDisabled
	}
	return identity.Validate()
}
