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

// VerifiedDocReader returns owned content verified against every current,
// repository-owned dependency. The hash selects an object; callers joining a
// snapshot must also compare its DocumentRef. This proof permits reading, not
// publication or reuse in a later request.
type VerifiedDocReader interface {
	ReadVerifiedDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.VerifiedSessionDoc, error)
}

// StoredCaptureComparator checks complete same-session event containment from
// currently owned, hash-verified bytes. A read/search projection alone is not
// evidence for deleting an older capture. Callers retain reference guards and
// the repository transaction around verification and collection.
type StoredCaptureComparator interface {
	CaptureSupersedes(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, domain.ProviderKind, string) (bool, error)
}
