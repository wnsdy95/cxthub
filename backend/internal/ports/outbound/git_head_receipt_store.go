package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitHeadReceiptReader interface {
	GetGitHeadReceipt(ctx context.Context, repo domain.ContentHash, origin string, version int64) (domain.GitHeadReceipt, error)
}

type GitHeadReceiptStore interface {
	GitHeadReceiptReader
	ListGitHeadReceipts(ctx context.Context, repo domain.ContentHash, origin string, afterVersion int64, limit int) ([]domain.GitHeadReceipt, error)
}
