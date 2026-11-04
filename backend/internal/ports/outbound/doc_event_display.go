package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocEventDisplayWorkReader interface {
	LoadObject(context.Context, domain.ContentHash) ([]byte, error)
	PrepareStep(context.Context, domain.DocEventDisplayPreparation, func(context.Context, int, int) ([]byte, error), ...domain.DocToolPreparationReader) (domain.DocEventDisplayStep, error)
}

type DocToolTreeWorkReader interface {
	ReadToolTreeNodes(context.Context, domain.DocToolTreeSource, int, int) ([]domain.DocToolTreeNode, error)
}

type DocToolKeyWorkReader interface {
	ReadToolKeyMembers(context.Context, domain.DocToolTreeSource, domain.DocToolKeyMapSource, int, *domain.DocToolKeyMember, int) ([]domain.DocToolKeyMember, error)
}

type DocToolOrderWorkReader interface {
	ReadToolOrderSource(context.Context, domain.DocToolTreeSource, domain.DocToolKeyMapSource, int) (domain.DocToolOrderSource, error)
	ReadToolOrderEntries(context.Context, domain.DocToolTreeSource, domain.DocToolKeyMapSource, domain.DocToolOrderSource, *domain.DocToolOrderEntry, int) ([]domain.DocToolOrderEntry, error)
	ReadToolOrderPlan(context.Context, domain.DocToolTreeSource, domain.DocToolKeyMapSource, domain.DocToolOrdersSource, int) (domain.DocToolOrderPlan, error)
}

type DocEventDisplayReader interface {
	ReadDocEventDisplayGeneration(context.Context, domain.DocDisplayTarget) (domain.DocDisplayProjectionGeneration, error)
	ReadDocEventDisplayPage(context.Context, domain.DocDisplayTarget, int) (domain.DocDisplayPage, error)
}

type DocEventDisplayStore interface {
	DocEventDisplayReader
	LoadDocEventDisplay(context.Context, domain.DocDisplayJob) (domain.DocEventDisplayPreparation, error)
	OpenDocEventDisplayWork(context.Context, domain.DocDisplayJob, domain.ContentHash) (DocEventDisplayWorkReader, error)
	SaveDocEventDisplay(context.Context, domain.DocDisplayJob, domain.DocEventDisplayStep) (domain.DocEventDisplayPreparation, error)
	CompleteDocEventDisplay(context.Context, domain.DocDisplayJob, domain.ContentHash, time.Time) error
}
