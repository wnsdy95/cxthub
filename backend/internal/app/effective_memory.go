package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const effectiveMemoryPageBytes = memoryquery.MaxPageBytes
const effectiveMemoryMaxItems = memoryquery.MaxItems

type effectiveMemoryCursor = memoryquery.Cursor

func normalizeEffectiveMemoryRequest(repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryRequest, error) {
	return memoryquery.NormalizeRequest(repo, request)
}

func (service *Service) QueryEffectiveMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	request, err := normalizeEffectiveMemoryRequest(repo, request)
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	queries, err := service.LiveMemoryQueries()
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	return queries.QueryEffectiveMemory(ctx, repo, request)
}

func (service *Service) queryEffectiveMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	queries, err := service.LiveMemoryQueries()
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	return queries.Project(ctx, repo, request)
}

func effectiveMemoryOffset(encoded string, state domain.ContentHash) (int, error) {
	return memoryquery.Offset(encoded, state)
}

func effectiveMemoryItems(digest domain.MemoryDigest, content string) ([]domain.EffectiveMemoryItem, error) {
	return memoryquery.EffectiveItems(digest, content)
}
