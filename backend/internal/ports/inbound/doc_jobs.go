package inbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// DocFinalization is a separate command/query boundary. Status contains no raw
// context or manifest; authorization is the same as context upload.
type DocFinalization interface {
	SubmitDocFinalization(context.Context, domain.ContentHash, ChunkedDoc) (DocFinalizationStatus, error)
	GetDocFinalization(context.Context, domain.ContentHash, string) (DocFinalizationStatus, error)
}
type DocFinalizationStatus struct {
	ID        string             `json:"id"`
	DocHash   domain.ContentHash `json:"doc_hash"`
	State     string             `json:"state"`
	Reason    string             `json:"reason,omitempty"`
	UpdatedAt time.Time          `json:"updated_at"`
}
