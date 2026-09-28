package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// StoredDocVerifier verifies the current repository-owned body without exposing
// mutable decoded CIR to a metadata-only command. Cache hints can avoid repeated
// schema parsing only after rereading and hashing actual bytes; an index, file
// timestamp, grant from another repo or old successful read is never sufficient.
type StoredDocVerifier interface {
	VerifyStoredDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.VerifiedDocReference, error)
}

// StoredCaptureComparator checks complete same-session event containment from
// currently owned, hash-verified bytes. A read/search projection alone is not
// evidence for deleting an older capture. Callers retain reference guards and
// the repository transaction around verification and collection.
type StoredCaptureComparator interface {
	CaptureSupersedes(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, domain.ProviderKind, string) (bool, error)
}
