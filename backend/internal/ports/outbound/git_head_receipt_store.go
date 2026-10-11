package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitHeadReceiptStore interface {
	GetGitHeadReceipt(ctx context.Context, repo domain.ContentHash, origin string, version int64) (domain.GitHeadReceipt, error)
	ListGitHeadReceipts(ctx context.Context, repo domain.ContentHash, origin string, afterVersion int64, limit int) ([]domain.GitHeadReceipt, error)
}
