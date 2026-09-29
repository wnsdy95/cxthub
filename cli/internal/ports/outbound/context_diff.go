package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// ContextDocumentReader supplies immutable stored documents only.
type ContextDocumentReader interface {
	GetDoc(context.Context, domain.ContentHash) (domain.SessionDoc, error)
}
