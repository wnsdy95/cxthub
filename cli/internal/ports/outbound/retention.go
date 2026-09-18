package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type CaptureCollection struct {
	RepoID      string             `json:"repo_id"`
	Previous    domain.ContentHash `json:"previous"`
	Replacement domain.ContentHash `json:"replacement"`
}

// ObjectRetention protects advertised objects for a complete read/install.
// It coordinates collection, not mutable ref updates or remote transactions.
// Every store that supports automatic capture collection must implement it.
type ObjectRetention interface {
	WithObjectsRetained(context.Context, func() error) error
	TryCollectObjects(context.Context, func() error) (bool, error)
	QueueCaptureCollection(context.Context, CaptureCollection) error
	CaptureCollections(context.Context, string, int) ([]CaptureCollection, error)
	CompleteCaptureCollection(context.Context, CaptureCollection) error
}
