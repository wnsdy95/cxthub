package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type PRHistoryReader interface {
	GetPRHistoryCheckpoint(context.Context, domain.ContentHash, string, int64, int) (domain.PRHistoryCheckpoint, error)
	GetPRRefObservation(context.Context, domain.ContentHash, string, string) (domain.GitRefObservation, error)
	ListPRParticipations(context.Context, domain.ContentHash, string, int) ([]domain.PRParticipation, error)
}

type PRHistoryStore interface {
	PRHistoryReader
	ListPRHistoryJobs(context.Context, domain.ContentHash, string, string, int64, time.Time, int) ([]domain.PRPromotionJob, error)
	SavePRHistoryCheckpoint(context.Context, domain.PRHistoryCheckpoint, int64) error
	ListPRRefObservations(context.Context, domain.ContentHash, string, int64, int, string, int) ([]domain.GitRefObservation, error)
	PutPRParticipation(context.Context, domain.PRParticipation) error
}
