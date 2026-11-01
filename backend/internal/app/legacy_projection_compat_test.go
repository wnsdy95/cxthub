package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphProjectionKey = repositoryquery.GraphProjectionKey
type branchProjectionKey = repositoryquery.BranchProjectionKey

const (
	graphProjectionCacheBytes    = repositoryquery.GraphProjectionCacheBytes
	graphProjectionExpandedBytes = repositoryquery.GraphProjectionExpandedBytes
)

var errGraphProjectionCacheLimit = repositoryquery.ErrGraphProjectionCacheLimit

func encodeGraphProjection(ctx context.Context, graph domain.GraphState) ([]byte, error) {
	return repositoryquery.EncodeGraphProjection(ctx, graph)
}

func decodeGraphProjection(ctx context.Context, encoded []byte) (domain.GraphState, error) {
	return repositoryquery.DecodeGraphProjection(ctx, encoded)
}
