package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitScanQuery interface {
	ListScans(context.Context, domain.ContentHash, string, int) (domain.GitScanPage, error)
}
type GitScans interface {
	GitScanQuery
	RetryScan(context.Context, domain.ContentHash, string) error
	ObservePush(context.Context, string, string, string, string, bool, string) (int, error)
}
