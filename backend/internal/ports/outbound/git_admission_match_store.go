package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitAdmissionMatchStore interface {
	GetGitAdmissionMatch(context.Context, domain.ContentHash, string, domain.GitProviderObservation, int) (domain.GitAdmissionMatch, error)
	GitAssociationPublicationUpperID(context.Context, domain.ContentHash, string) (string, error)
	SaveGitAdmissionMatch(context.Context, domain.GitAdmissionMatch, int64, domain.GitHeadReceipt) error
}
