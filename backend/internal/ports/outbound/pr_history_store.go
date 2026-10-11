package outbound

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type PRHistoryStore interface {
	ListPRHistoryJobs(context.Context, domain.ContentHash, string, string, int64, time.Time, int) ([]domain.PRPromotionJob, error)
	GetPRHistoryCheckpoint(context.Context, domain.ContentHash, string, int64, int) (domain.PRHistoryCheckpoint, error)
	SavePRHistoryCheckpoint(context.Context, domain.PRHistoryCheckpoint, int64) error
	ListPRRefObservations(context.Context, domain.ContentHash, string, int64, int, string, int) ([]domain.GitRefObservation, error)
	GetPRRefObservation(context.Context, domain.ContentHash, string, string) (domain.GitRefObservation, error)
	PutPRParticipation(context.Context, domain.PRParticipation) error
	ListPRParticipations(context.Context, domain.ContentHash, string, int) ([]domain.PRParticipation, error)
}
