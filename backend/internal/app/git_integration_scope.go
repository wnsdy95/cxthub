package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func repositoryReadForRepoWithWork[Result any](ctx context.Context, work *repositoryWork, repo domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), work.supported)
	return repositoryReadWithWork(ctx, work, func(bound context.Context) (Result, error) {
		if err := work.checkDocumentIdentity(bound, repo, true); err != nil {
			var zero Result
			return zero, err
		}
		return read(bound)
	})
}
