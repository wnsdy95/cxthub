package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type ContextDiffInput struct {
	Cwd    string
	Staged bool
}
type ContextDiffQuery interface {
	Diff(context.Context, ContextDiffInput) (domain.ContextDiff, error)
}
