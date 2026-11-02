package app

import "github.com/wnsdy95/cxthub/backend/internal/ports/outbound"

type documentQueryUnusedStore struct {
	outbound.MetadataStore
	outbound.BlobStore
}

func newUnusedDocumentQueryService() *Service {
	storage := &documentQueryUnusedStore{}
	return NewService(storage, storage, nil, nil, nil)
}
