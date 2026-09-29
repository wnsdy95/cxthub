package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type AgentHistoryReader interface {
	ReadAgentHistoryPage(context.Context, domain.ContentHash, domain.ContentHash, domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error)
}
