package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryMemorySourceGenerationStore interface {
	ReadRepositoryMemorySourceGeneration(context.Context, domain.ContentHash) (domain.RepositoryMemorySourceGeneration, error)
}
