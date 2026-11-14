package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// StoredDocVerifier verifies the current repository-owned body without exposing
// mutable decoded CIR to a metadata-only command. Cache hints can avoid repeated
// schema parsing only after rereading and hashing actual bytes; an index, file
// timestamp, grant from another repo or old successful read is never sufficient.
// FinalizedDocAttestor is the one alternative a publishing command may accept.
type StoredDocVerifier interface {
	VerifyStoredDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.VerifiedDocReference, error)
}

// FinalizedDocAttestor reports whether a completed finalization job already
// verified exactly the document representation that the repository stores now.
// It reads ownership and the job record, never chunk bodies, so its cost does
// not grow with the document. found is false whenever no such record matches,
// including for a missing or unowned document; the caller then verifies the
// bytes with StoredDocVerifier, which stays the only source of a refusal. An
// error means the record could not be read.
type FinalizedDocAttestor interface {
	FinalizedDocReference(context.Context, domain.ContentHash, domain.ContentHash) (proof domain.VerifiedDocReference, found bool, err error)
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
