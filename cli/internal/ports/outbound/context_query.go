package outbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type ContextQueryReader interface {
	QueryContext(context.Context, string, domain.ContextSelection) (domain.ContextQueryView, error)
}
