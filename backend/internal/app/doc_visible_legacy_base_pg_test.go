//go:build postgres

package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docVisibleLegacyBasePGStore struct {
	*docViewPGStore
	test        *testing.T
	base        domain.ContentHash
	parentOpens int
	parentPages int
}

type docVisibleLegacyBasePGRead struct {
	outbound.DocRangeGenerationRead
	storage *docVisibleLegacyBasePGStore
	hash    domain.ContentHash
}

func (storage *docVisibleLegacyBasePGStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	read, err := storage.docViewPGStore.OpenDocRangeGeneration(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	if hash == storage.base {
		storage.parentOpens++
	}
	return &docVisibleLegacyBasePGRead{DocRangeGenerationRead: read, storage: storage, hash: hash}, nil
}

func (read *docVisibleLegacyBasePGRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	if read.hash == read.storage.base {
		read.storage.parentPages++
	}
	return read.DocRangeGenerationRead.ReadPage(ctx, position)
}

func (storage *docVisibleLegacyBasePGStore) GetDoc(_ context.Context, _, hash domain.ContentHash) (domain.SessionDoc, error) {
	storage.test.Error("foreground materialized a complete document", hash)
	return domain.SessionDoc{}, domain.ErrIntegrity
}

func (storage *docVisibleLegacyBasePGStore) GetDocManifest(_ context.Context, _, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.test.Error("foreground loaded a complete archival manifest", hash)
	return domain.DocChunkManifest{}, domain.ErrIntegrity
}

func (storage *docVisibleLegacyBasePGStore) DocReadIndex(_ context.Context, _, hash domain.ContentHash) (domain.DocReadIndex, error) {
	storage.test.Error("foreground loaded a full legacy event index", hash)
	return domain.DocReadIndex{}, domain.ErrIntegrity
}

func (storage *docVisibleLegacyBasePGStore) DocRangeIndex(_ context.Context, _, hash domain.ContentHash) (domain.DocRangeIndex, error) {
	storage.test.Error("foreground loaded a full range index", hash)
	return domain.DocRangeIndex{}, domain.ErrIntegrity
}

func (storage *docVisibleLegacyBasePGStore) EnsureDocRangeIndex(_ context.Context, _, hash domain.ContentHash) error {
	storage.test.Error("foreground built a document range index", hash)
	return domain.ErrIntegrity
}
