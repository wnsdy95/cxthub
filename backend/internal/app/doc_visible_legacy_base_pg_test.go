//go:build postgres

package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docVisibleLegacyBasePGStore struct {
	*docViewPGStore
	test        *testing.T
	base        domain.ContentHash
	parentOpens int
	parentPages int
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
