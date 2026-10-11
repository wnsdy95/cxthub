package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitAssociationStatusEvidence struct {
	Publication *domain.HistoryEvent
	Admission   *domain.GitContributionAdmission
	Work        *domain.GitContributionWork
	Supplement  *domain.GitContributionSupplement
	Proof       *domain.GitContributionProof
	Memory      *domain.GitContributionMemory
}

type GitAssociationStatusStore interface {
	ReadGitAssociationStatus(context.Context, domain.ContentHash, string, domain.GitAssociationStatusRequest) ([]GitAssociationStatusEvidence, error)
}
