package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GraphStateQuery interface {
	QueryGraphState(context.Context, domain.ContentHash, string) (domain.GraphState, error)
}
