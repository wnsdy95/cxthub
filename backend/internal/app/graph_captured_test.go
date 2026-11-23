package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type poisonedGraphMetadata struct{ outbound.MetadataStore }

func (*poisonedGraphMetadata) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	panic("projection read live repository metadata")
}

func (*poisonedGraphMetadata) InReadOnlyTransaction(context.Context) bool {
	panic("projection inspected live transaction state")
}

type capturedGraphRepositoryOnly struct {
	outbound.MetadataStore
	metadata domain.Repo
}

func (reader *capturedGraphRepositoryOnly) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return reader.metadata, nil
}
