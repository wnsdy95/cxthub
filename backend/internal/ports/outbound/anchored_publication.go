package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// AnchoredPublicationStore keeps, for each session, the ranges of its own
// records that are open to the team. A session's list is replaced whole; the
// application only ever adds to it.
type AnchoredPublicationStore interface {
	// GetAnchoredPublication answers an empty list for a session nothing is open of.
	GetAnchoredPublication(context.Context, domain.ContentHash, domain.ProviderKind, string) ([]domain.AnchoredPublishedRange, error)
	PutAnchoredPublication(context.Context, domain.ContentHash, domain.AnchoredPublication) error
	// ListAnchoredPublications lists every session of the repository that has an open range, by tool then identifier.
	ListAnchoredPublications(context.Context, domain.ContentHash) ([]domain.AnchoredPublication, error)
}
