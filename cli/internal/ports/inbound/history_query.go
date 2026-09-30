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

// LocalHistoryObserver supplies one fresh HEAD observation and the fingerprint
// of the entire snapshot/ref catalog, including unreachable metadata. It does
// not guarantee a stable observation. Callers must compare complete working
// observations before returning a result, and again after reading diff bodies.
// QueryHistory remains the independently fenced public query.
type LocalHistoryObserver interface {
	ObserveLocalHistory(context.Context, string) (domain.HistoryQueryResult, domain.ContentHash, error)
}
