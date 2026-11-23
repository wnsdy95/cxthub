package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type findFragmentUnindexedStore struct {
	outbound.BlobStore
	doc   domain.SessionDoc
	reads int
}

func (storage *findFragmentUnindexedStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	storage.reads++
	return storage.doc, nil
}
