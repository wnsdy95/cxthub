package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocDisplayReader interface {
	ReadDocDisplayGeneration(context.Context, domain.DocDisplayTarget) (domain.DocDisplayStringGeneration, error)
	ReadDocDisplayPage(context.Context, domain.DocDisplayTarget, int) (domain.DocDisplayPage, error)
}

type DocDisplayStore interface {
	DocDisplayReader
	EnqueueDocDisplay(context.Context, domain.DocDisplayTarget, time.Time) (domain.DocDisplayJob, error)
	ClaimDocDisplay(context.Context, time.Time, time.Duration) (domain.DocDisplayJob, error)
	RenewDocDisplay(context.Context, domain.DocDisplayJob, time.Time, time.Duration) error
	LoadDocDisplayJob(context.Context, domain.DocDisplayJob) (domain.DocDisplayPreparation, error)
	SaveDocDisplayDiscovery(context.Context, domain.DocDisplayJob, domain.DocFieldDiscoveryStep) (domain.DocDisplayPreparation, error)
	InitializeDocDisplay(context.Context, domain.DocDisplayJob, domain.DocDisplayStringSource) (domain.DocDisplayPreparation, error)
	SaveDocDisplayStep(context.Context, domain.DocDisplayJob, domain.ContentHash, domain.DocDisplayStringStep) (domain.DocDisplayPreparation, error)
	CompleteDocDisplay(context.Context, domain.DocDisplayJob, domain.ContentHash, time.Time) error
	FinishDocDisplay(context.Context, domain.DocDisplayJob, time.Time) error
}
