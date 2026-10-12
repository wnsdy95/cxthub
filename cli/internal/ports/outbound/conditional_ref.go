package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type ConditionalRefWriter interface {
	AppendRefConditional(ctx context.Context, repoID string, ref domain.Ref, expectedTarget domain.ContentHash, expectedRevision domain.RepositoryRevision) error
}
