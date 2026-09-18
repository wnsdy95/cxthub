package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type CodeApplicabilityQuery interface {
	QueryCodeApplicability(context.Context, domain.ContentHash, domain.CodeSelection) (domain.CodeApplicability, error)
}
