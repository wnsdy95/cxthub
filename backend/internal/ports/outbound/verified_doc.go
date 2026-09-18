package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// VerifiedDocStore accepts domain-validated immutable input without repeating
// canonicalization. Adapters still enforce repository ownership, stored-object
// integrity, and atomic blob/index publication. Legacy adapters use PutDoc.
type VerifiedDocStore interface {
	PutVerifiedDoc(context.Context, domain.ContentHash, domain.VerifiedSessionDoc) (bool, error)
}
