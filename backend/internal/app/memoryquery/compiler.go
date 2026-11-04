package memoryquery

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func BuildRead(ctx context.Context, queries *LiveQueries, repo, captureID domain.ContentHash, generation domain.RepositoryMemorySourceGeneration, selection domain.EffectiveMemorySelection, content string, dependencies []domain.MemoryReadDependency) (domain.MemoryReadBundle, error) {
	if err := ctx.Err(); err != nil {
		return domain.MemoryReadBundle{}, err
	}
	if queries == nil || queries.mode != LiveFrozen || queries.frozenGeneration == nil ||
		*queries.frozenGeneration != generation || generation.Graph.RepoID != repo {
		return domain.MemoryReadBundle{}, domain.ErrIntegrity
	}
	request := domain.EffectiveMemoryRequest{Selection: selection, Content: content}
	selected, err := queries.prepareEffectiveMemorySelection(ctx, repo, request)
	if err != nil {
		return domain.MemoryReadBundle{}, err
	}
	items, err := EffectiveItems(selected.digest, content)
	if err != nil {
		return domain.MemoryReadBundle{}, err
	}
	resolver, err := newMemoryIntegration(queries, selected.evidence, repo, selected.history)
	if err != nil {
		return domain.MemoryReadBundle{}, err
	}
	used := 0
	for ordinal, item := range items {
		if err := ctx.Err(); err != nil {
			return domain.MemoryReadBundle{}, err
		}
		if item.Kind == "code" {
			item, err = resolver.assessReadItem(ctx, item, selection.CodeCommit, selected.view.Revision)
			if err != nil {
				return domain.MemoryReadBundle{}, err
			}
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return domain.MemoryReadBundle{}, err
		}
		if len(raw) > MaxPageBytes || len(raw) > domain.MaxMemoryReadBytes-used {
			return domain.MemoryReadBundle{}, domain.ErrMemoryProjectionLimit
		}
		used += len(raw)
		items[ordinal] = item
	}
	selected.view.Total = len(items)
	selected.view.Items = nil
	selected.view.DeliveryStateHash, err = domain.EffectiveMemoryDeliveryStateHash(ctx, repo, selected.evidence.origin, selected.view, items)
	if err != nil {
		return domain.MemoryReadBundle{}, err
	}
	header := domain.MemoryReadHeader{Version: domain.MemoryReadVersion, Rules: domain.MemoryReadRules, CaptureID: captureID, Source: generation, Origin: selected.evidence.origin, View: selected.view, Dependencies: dependencies}
	return domain.BuildMemoryReadBundle(ctx, header, items)
}
