package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type HistoryQueryInput struct {
	Cwd string
	// Position pins a source independently of its logical branch for package reads.
	Position domain.ContentHash
	Ref      string
	Branch   string
	All      bool
	Retained bool
	Server   bool
}
type HistoryQuery interface {
	QueryHistory(context.Context, HistoryQueryInput) (domain.HistoryQueryResult, error)
}
