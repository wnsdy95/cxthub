package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type preparedMemoryHTTPBackendTrap struct {
	Backend
	scopes  int
	queries int
}

func (backend *preparedMemoryHTTPBackendTrap) WithMemoryReadResources(context.Context, domain.ContentHash, func(context.Context) error) error {
	backend.scopes++
	return domain.ErrIntegrity
}
