package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// StoredDocumentReferenceVerifier checks an explicitly selected identity
// against all current local bytes. Metadata equality and old receipts are not
// sufficient to verify a conversation root.
type StoredDocumentReferenceVerifier interface {
	VerifyStoredDocReference(context.Context, domain.DocumentRef) error
}

// DocumentReferenceReader materializes verified content without relabeling its
// identity. Capture and publication capabilities are separate from this read.
type DocumentReferenceReader interface {
	GetDocReference(context.Context, domain.DocumentRef) (domain.SessionDoc, error)
}
