package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// HistoryDocumentProofStore scopes metadata evidence to one synchronous request.
// The callback context must be used for preparation and final publication. A
// nil proof selects the original verification path for an existing write
// transaction. Unsupported adapters need not implement this capability.
type HistoryDocumentProofStore interface {
	WithinHistoryDocumentProof(context.Context, domain.ContentHash, func(context.Context, HistoryDocumentProof) error) error
}

// HistoryDocumentProof binds full current-byte verification to tuple versions.
// Capture follows full verification in the SAME owned RR read-only snapshot.
// Pin compares and locks the complete captured closure in the repository write
// transaction, before an operation-local verification map can be used. Pins
// last until commit/rollback. The callback must not mutate those dependencies
// after Pin. Neither this proof nor the map may escape to another request.
type HistoryDocumentProof interface {
	Capture(context.Context, []domain.Snapshot) error
	Pin(context.Context) error
}
