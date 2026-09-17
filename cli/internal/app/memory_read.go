package app

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// MemoryReader is the complete read-only dependency of lineage projection.
// In particular the offline MCP adapter cannot acquire writes through it.
type MemoryReader interface {
	GetSnapshot(context.Context, domain.ContentHash) (domain.Snapshot, error)
	GetMemory(context.Context, domain.ContentHash) (domain.MemoryDigest, error)
}

// ReadProjectedMemory is the read-only entry point for offline MCP. A local
// rewind uses its exact pinned object. Current selections project every parent
// frontier and reject incomplete/changing replicas rather than hiding gaps.
func ReadProjectedMemory(ctx context.Context, store MemoryReader, id domain.ContentHash) (domain.MemoryDigest, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.MemoryDigest{}, false, err
	}
	if d, pinned, err := selectedMemory(ctx, store, id); pinned || err != nil {
		return d, pinned && d.SnapshotID != "", err
	}
	for attempt := 0; attempt < memoryProjectionReadAttempts; attempt++ {
		before, err := readMemoryProjectionState(ctx, store, id)
		if err != nil {
			return domain.MemoryDigest{}, false, err
		}
		d, found, complete := memoryProjectionFromDetailed(ctx, store, id)
		after, err := readMemoryProjectionState(ctx, store, id)
		if err != nil {
			return domain.MemoryDigest{}, false, err
		}
		if !sameMemoryProjectionState(before, after) {
			continue
		}
		if !complete || !after.lineageComplete {
			return domain.MemoryDigest{}, false, fmt.Errorf("local memory lineage is incomplete; pull missing context or use cloud MCP")
		}
		return d, found, nil
	}
	return domain.MemoryDigest{}, false, fmt.Errorf("%w: memory lineage changed during read; retry", domain.ErrSyncConflict)
}
