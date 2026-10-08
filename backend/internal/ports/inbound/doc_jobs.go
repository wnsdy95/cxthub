package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// DocFinalization is a separate command/query boundary. Status contains no raw
// context or manifest; authorization is the same as context upload.
type DocFinalization interface {
	SubmitDocFinalization(context.Context, domain.ContentHash, ChunkedDoc) (DocFinalizationStatus, error)
	GetDocFinalization(context.Context, domain.ContentHash, string) (DocFinalizationStatus, error)
}

// Shared receipt metadata. App projection must preserve the explicit identity.
type DocFinalizationStatus = domain.DocFinalizationStatus
