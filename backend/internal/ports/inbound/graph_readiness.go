package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GraphReadinessQuery interface {
	GetGraphReadiness(context.Context, domain.ContentHash) (domain.GraphReadiness, error)
}
