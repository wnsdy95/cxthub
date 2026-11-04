package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type ConversationFindPreparationRequester interface {
	RequestConversationFindPreparation(context.Context, string, domain.ConversationFindPreparationRequest) error
}
