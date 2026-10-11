package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitAssociationDiscoveryStore interface {
	GetGitAssociationDiscovery(context.Context, domain.ContentHash, string) (domain.GitAssociationDiscovery, error)
	SaveGitAssociationDiscovery(context.Context, domain.GitAssociationDiscovery, int64, []domain.HistoryEvent) error
	ListGitAssociationPublications(context.Context, domain.ContentHash, string, string, int) ([]domain.HistoryEvent, error)
}
