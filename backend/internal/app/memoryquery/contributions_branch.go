package memoryquery

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func ProjectBranchWithPlan(ctx context.Context, source outbound.MemoryProjectionReader, repo domain.ContentHash, inclusion domain.BranchContext, plan ContributionPlan) (domain.MemoryProjection, error) {
	allowed := map[domain.ContentHash]bool{}
	for _, id := range inclusion.SnapshotIDs {
		allowed[id] = true
	}
	rows := make([]domain.Snapshot, 0, len(allowed)+1)
	for _, snap := range plan.Snapshots {
		if !allowed[snap.ID] {
			continue
		}
		grafts := make([]domain.ContentHash, 0, len(snap.GraftParents))
		for _, parent := range snap.GraftParents {
			if allowed[parent] {
				grafts = append(grafts, parent)
			}
		}
		snap.GraftParents = grafts
		rows = append(rows, snap)
	}
	key, _ := json.Marshal(struct {
		Version   int
		Inclusion domain.BranchContext
	}{1, inclusion})
	id := domain.HashContent(key)
	if allowed[id] {
		return domain.MemoryProjection{}, domain.ErrIntegrity
	}
	rows = append(rows, domain.Snapshot{ID: id, RepoID: repo, Parents: inclusion.Roots})
	result, err := ProjectMemory(ctx, contributionProjectionSource{source: source, rows: rows}, repo, id)
	result.Digest.SnapshotID = inclusion.SnapshotID
	result.Inclusion = &inclusion
	if err != nil {
		return result, err
	}
	return plan.Merge(ctx, result)
}

type contributionProjectionSource struct {
	source outbound.MemoryProjectionReader
	rows   []domain.Snapshot
}

func (source contributionProjectionSource) ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error) {
	return source.rows, nil
}

func (source contributionProjectionSource) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	return source.source.GetMemory(ctx, repo, hash)
}
