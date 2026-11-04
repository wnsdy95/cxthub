package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RepositoryViewSourceStore interface {
	RepositoryViewSourceChanges(context.Context, domain.ContentHash, domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error)
}

type RepositoryGraphSourceStore interface {
	RepositoryGraphSourceChanges(context.Context, domain.ContentHash, domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error)
}

type RepositoryGraphSourceGenerationStore interface {
	ReadRepositoryGraphSourceGeneration(context.Context, domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error)
}
