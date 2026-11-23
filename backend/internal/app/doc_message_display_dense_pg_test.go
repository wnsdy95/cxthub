//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type denseDocDisplayBlobProbe struct {
	*store.PostgresStore
	rawReads int
	deny     bool
}

func (probe *denseDocDisplayBlobProbe) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	probe.rawReads++
	if probe.deny {
		return nil, domain.ErrIntegrity
	}
	return probe.PostgresStore.ReadConversationChunk(ctx, repo, hash, length)
}

func (probe *denseDocDisplayBlobProbe) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	probe.rawReads++
	if probe.deny {
		return nil, domain.ErrIntegrity
	}
	return probe.PostgresStore.GetChunk(ctx, repo, hash)
}

func (probe *denseDocDisplayBlobProbe) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	probe.rawReads++
	if probe.deny {
		return domain.SessionDoc{}, domain.ErrIntegrity
	}
	return probe.PostgresStore.GetDoc(ctx, repo, hash)
}
