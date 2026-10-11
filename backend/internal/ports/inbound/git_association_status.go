package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitAssociationStatusQuery interface {
	GetGitAssociationStatus(context.Context, domain.ContentHash, domain.GitAssociationStatusRequest) (domain.GitAssociationStatusPage, error)
}
