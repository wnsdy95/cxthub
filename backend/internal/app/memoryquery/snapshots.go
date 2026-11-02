package memoryquery

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type SnapshotScope interface {
	inbound.MemoryReadScope
	WithinMemorySnapshot(context.Context, domain.ContentHash, func(context.Context) error) error
}

type SnapshotMetadata interface {
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	GetMemoryMeta(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error)
}

type SnapshotConfig struct {
	Scope      SnapshotScope
	Metadata   SnapshotMetadata
	Objects    outbound.MemoryObjectReader
	Projection outbound.MemoryProjectionReader
}

type SnapshotQueries struct {
	scope      SnapshotScope
	metadata   SnapshotMetadata
	objects    outbound.MemoryObjectReader
	projection outbound.MemoryProjectionReader
}

var _ inbound.MemorySnapshotQueries = (*SnapshotQueries)(nil)

func NewSnapshots(config SnapshotConfig) (*SnapshotQueries, error) {
	for _, dependency := range []any{config.Scope, config.Metadata, config.Objects, config.Projection} {
		if absent(dependency) {
			return nil, fmt.Errorf("%w: snapshot memory query dependency is required", domain.ErrCatalogUnsupported)
		}
	}
	return &SnapshotQueries{scope: config.Scope, metadata: config.Metadata, objects: config.Objects, projection: config.Projection}, nil
}

func (queries *SnapshotQueries) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if queries == nil || absent(queries.scope) {
		return domain.ErrCatalogUnsupported
	}
	return queries.scope.WithMemoryReadResources(ctx, repo, read)
}

func (queries *SnapshotQueries) GetMemoryObject(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	return readMemorySnapshot(ctx, queries, repo, hash, func(current context.Context) (domain.MemoryDigest, error) {
		return queries.objects.GetMemory(current, repo, hash)
	})
}

func (queries *SnapshotQueries) GetMemoryDigest(ctx context.Context, repo, id domain.ContentHash) (domain.MemoryDigest, error) {
	return readMemorySnapshot(ctx, queries, repo, id, func(current context.Context) (domain.MemoryDigest, error) {
		snapshot, err := queries.metadata.GetSnapshot(current, repo, id)
		if err != nil {
			return domain.MemoryDigest{}, err
		}
		if snapshot.MemoryHash == "" {
			return queries.metadata.GetMemoryMeta(current, repo, id)
		}
		digest, err := queries.objects.GetMemory(current, repo, snapshot.MemoryHash)
		if err != nil {
			return domain.MemoryDigest{}, err
		}
		if digest.SnapshotID != id {
			return domain.MemoryDigest{}, fmt.Errorf("%w: memory %s targets snapshot %s, want %s", domain.ErrIntegrity, snapshot.MemoryHash, digest.SnapshotID, id)
		}
		return digest, nil
	})
}

func (queries *SnapshotQueries) GetMemoryProjection(ctx context.Context, repo, id domain.ContentHash) (domain.MemoryProjection, error) {
	return readMemorySnapshot(ctx, queries, repo, id, func(current context.Context) (domain.MemoryProjection, error) {
		return ProjectMemory(current, queries.projection, repo, id)
	})
}

func readMemorySnapshot[Result any](ctx context.Context, queries *SnapshotQueries, repo, id domain.ContentHash, read func(context.Context) (Result, error)) (Result, error) {
	var result Result
	err := queries.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		return queries.scope.WithinMemorySnapshot(bound, repo, func(current context.Context) error {
			for _, hash := range []domain.ContentHash{repo, id} {
				if err := domain.ValidateContentHash(hash); err != nil {
					return err
				}
			}
			var err error
			result, err = read(current)
			return err
		})
	})
	if err != nil {
		var zero Result
		return zero, err
	}
	return result, nil
}
