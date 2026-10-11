package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const PRJobAssociationReadLimit = 4096

type PRJobAssociationEvidence struct {
	Checkpoints            []domain.PRHistoryCheckpoint
	CheckpointsComplete    bool
	Participations         []domain.PRParticipation
	ParticipationsComplete bool
	Contributions          []domain.PRContribution
	ContributionsComplete  bool
}

type PRJobAssociationStore interface {
	ReadPRJobAssociationEvidence(context.Context, domain.ContentHash, []int) (PRJobAssociationEvidence, error)
}
