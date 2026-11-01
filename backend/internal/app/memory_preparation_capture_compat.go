package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (service *Service) CaptureEffectiveMemorySource(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest, previous *CapturedGraphSource) (*CapturedEffectiveMemory, error) {
	return service.memoryPreparation().CaptureEffectiveMemorySource(ctx, repo, request, previous)
}

func (service *Service) captureEffectiveMemorySource(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest, previous *CapturedGraphSource, delta *memoryGraphDelta, reference func(context.Context) error) (*CapturedEffectiveMemory, error) {
	return service.memoryPreparation().captureEffectiveMemorySource(ctx, repo, request, previous, delta, reference)
}

func (service *Service) ValidateCapturedMemorySource(ctx context.Context, repo domain.ContentHash, captured *CapturedEffectiveMemory) error {
	return service.memoryPreparation().ValidateCapturedMemorySource(ctx, repo, captured)
}
