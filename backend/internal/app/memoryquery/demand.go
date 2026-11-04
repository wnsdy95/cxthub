package memoryquery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *PreparedQueries) RequestPreparedMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) error {
	encoded := request.Cursor
	request.Cursor = ""
	target, err := ReadTarget(repo, request)
	if err != nil || encoded == "" {
		return domain.ErrValidation
	}
	cursor, err := DecodePreparedCursor(encoded, target)
	if err != nil || cursor.Generation != "" || cursor.Continuation != "" {
		return domain.ErrValidation
	}
	return queries.scope.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		if !queries.configured[repo] || !queries.demands[repo] {
			return domain.ErrCatalogUnsupported
		}
		return queries.requester.RequestPrepared(bound, target, cursor.Requirement)
	})
}
