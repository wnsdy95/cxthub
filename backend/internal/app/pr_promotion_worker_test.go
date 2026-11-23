package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type wave45PRBlobs struct {
	*store.FSStore
	verify func(context.Context)
}

func (blobs *wave45PRBlobs) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	if blobs.verify != nil {
		blobs.verify(ctx)
	}
	return blobs.FSStore.VerifyStoredDoc(ctx, repo, hash)
}
