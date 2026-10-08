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

// RootDocumentStore installs a fully verified direct canonical root manifest.
// Its bounded bodies must already be staged, and installation grants no graph
// adoption or outbound publication authority.
type RootDocumentStore interface {
	PutConversationManifest(context.Context, domain.DocumentRepresentation) error
}

// DocumentPublicationPreflight checks current peer policy before any writes.
// A missing port cannot authorize publication of existing nonlegacy objects.
type DocumentPublicationPreflight interface {
	PreflightDocumentReferences(context.Context, string, []domain.DocumentRef) error
}
