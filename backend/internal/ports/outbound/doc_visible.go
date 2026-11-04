package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocVisibleGenerationRead interface {
	Descriptor() domain.DocVisibleGeneration
	Binding() domain.DocReadGenerationBinding
	Unit(context.Context, string, int, bool) (domain.DocVisibleUnit, int, error)
}

type DocVisibleGenerationStore interface {
	RepositoryTransactionState
	OpenDocVisibleGeneration(context.Context, domain.ContentHash, domain.ContentHash) (DocVisibleGenerationRead, error)
}

type DocVisiblePreparationStore interface {
	HasDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error)
	EnqueueDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error)
}
