package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitPublicationLookupStore interface {
	ListGitAdmissionRefs(context.Context, domain.ContentHash) ([]domain.Ref, error)
	GetGitHistoryEvent(context.Context, domain.ContentHash, string) (domain.HistoryEvent, error)
	ListGitBranchBindings(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error)
	ListGitPublicationWitnesses(context.Context, domain.ContentHash, domain.GitPublicationWitnessQuery) ([]domain.HistoryEvent, error)
	FindGitForeignHeadOwner(context.Context, domain.ContentHash, string, string) (domain.HistoryEvent, error)
}
