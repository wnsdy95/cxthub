package documentworker

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type displayBatchTestScope struct {
	*wave38DisplayScope
	afterRead func()
}

func (scope *displayBatchTestScope) WithinRepositoryRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	err := scope.wave38DisplayScope.WithinRepositoryRead(ctx, repo, read)
	if scope.afterRead != nil {
		scope.afterRead()
	}
	return err
}
