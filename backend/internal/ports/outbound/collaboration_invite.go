package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Writes run within IdentityTransactions along with membership and audit.
type CollaborationInviteStore interface {
	PutCollaborationInvite(context.Context, domain.CollaborationInvite) error
	GetCollaborationInvite(context.Context, string) (domain.CollaborationInvite, error)
	ListCollaborationInvites(context.Context, string, string, string) ([]domain.CollaborationInvite, error)
}
