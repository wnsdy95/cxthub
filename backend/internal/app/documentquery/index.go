package documentquery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *Queries) ReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	ctx = queries.readContext(ctx)
	return queries.docReadIndex(ctx, repo, hash)
}
