package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type yieldingPreparationMetadata struct {
	streamingPreparationMetadata
	nested bool
}

func (metadata yieldingPreparationMetadata) InTransaction(context.Context) bool {
	return metadata.nested
}

type yieldingRootPublication struct {
	resources outbound.RootPublicationResources
	complete  func(context.Context) error
	close     func() error
}

func (publication *yieldingRootPublication) RetainedResources() (outbound.RootPublicationResources, error) {
	return publication.resources, nil
}

func (publication *yieldingRootPublication) Complete(ctx context.Context, _ domain.DocFinalizationJob, _ time.Time) error {
	return publication.complete(ctx)
}

func (publication *yieldingRootPublication) Close() error {
	return publication.close()
}
