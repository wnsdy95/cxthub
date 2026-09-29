package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type WorkingStateQuery interface {
	Status(context.Context, string) (domain.WorkingState, error)
}
