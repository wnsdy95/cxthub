//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type historyCommandsStorePG struct {
	*documentPublicationProbePG
	afterVerify func(context.Context) error
}

func (storage *historyCommandsStorePG) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	proof, err := storage.PostgresStore.VerifyStoredDoc(ctx, repo, hash)
	if err == nil && storage.afterVerify != nil {
		err = storage.afterVerify(ctx)
	}
	return proof, err
}
