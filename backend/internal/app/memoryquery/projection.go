package memoryquery

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const MaxProjectionSnapshots = contextprojection.MaxProjectionSnapshots
const MaxProjectionEdges = 1 << 18
const MaxProjectionMemoryBytes = 64 << 20
const MaxProjectionCacheBytes = 8 << 20

func ProjectMemory(ctx context.Context, source outbound.MemoryProjectionReader, repoID, snapshotID domain.ContentHash) (domain.MemoryProjection, error) {
	if err := validateProjectionHashes(repoID, snapshotID); err != nil {
		return domain.MemoryProjection{}, err
	}
	reader := NewProjectionReader(source, repoID)
	for attempt := 0; attempt < 3; attempt++ {
		before, err := reader.readState(ctx, snapshotID)
		if err != nil {
			return domain.MemoryProjection{}, err
		}
		digest, found, complete, walkErr := ProjectLineage(ctx, reader, snapshotID)
		if reader.err != nil {
			return domain.MemoryProjection{}, reader.err
		}
		if walkErr != nil {
			return domain.MemoryProjection{}, walkErr
		}
		if !complete {
			return domain.MemoryProjection{}, fmt.Errorf("%w: incomplete memory projection", domain.ErrIntegrity)
		}
		after, err := reader.readState(ctx, snapshotID)
		if err != nil {
			return domain.MemoryProjection{}, err
		}
		if before != after {
			continue
		}

		digest.SnapshotID = snapshotID
		digest.PreviousMemoryHash = ""
		digest.GraftCoverage = nil
		return domain.MemoryProjection{Digest: digest, Found: found, StateHash: after}, nil
	}
	return domain.MemoryProjection{}, fmt.Errorf("%w: memory projection changed during read; retry", domain.ErrConflict)
}

type ProjectionReader struct {
	source      outbound.MemoryProjectionReader
	repoID      domain.ContentHash
	snapshots   map[domain.ContentHash]domain.Snapshot
	memories    map[domain.ContentHash]domain.MemoryDigest
	weights     map[domain.ContentHash]int
	order       []domain.ContentHash
	validated   map[domain.ContentHash]bool
	cacheBytes  int
	cacheLimit  int
	objectLimit int
	bytes       int
	err         error
}

func NewProjectionReader(source outbound.MemoryProjectionReader, repoID domain.ContentHash) *ProjectionReader {
	return &ProjectionReader{source: source, repoID: repoID, memories: map[domain.ContentHash]domain.MemoryDigest{}}
}

func (reader *ProjectionReader) BytesRead() int {
	return reader.bytes
}

func (reader *ProjectionReader) readState(ctx context.Context, root domain.ContentHash) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	snapshots, err := reader.source.ListSnapshots(ctx, reader.repoID, "")
	if err != nil {
		return "", err
	}
	reader.snapshots = make(map[domain.ContentHash]domain.Snapshot, len(snapshots))
	for _, snapshot := range snapshots {
		reader.snapshots[snapshot.ID] = snapshot
	}

	state := map[domain.ContentHash]uint8{}
	edges := 0
	var visit func(domain.ContentHash) error
	visit = func(snapshotID domain.ContentHash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if state[snapshotID] == 2 {
			return nil
		}
		if state[snapshotID] == 1 {
			return fmt.Errorf("%w: cycle in memory lineage", domain.ErrIntegrity)
		}
		if len(state) >= MaxProjectionSnapshots {
			return fmt.Errorf("%w: exceeds %d snapshots; select a narrower ref", domain.ErrMemoryProjectionLimit, MaxProjectionSnapshots)
		}
		snapshot, ok := reader.snapshots[snapshotID]
		if !ok {
			return fmt.Errorf("%w: missing memory lineage snapshot %s", domain.ErrIntegrity, snapshotID)
		}
		if snapshot.RepoID != reader.repoID {
			return fmt.Errorf("%w: memory lineage repository mismatch", domain.ErrIntegrity)
		}
		if err := domain.ValidateContentHash(snapshotID); err != nil {
			return err
		}
		if err := domain.ValidateOptionalContentHash(snapshot.MemoryHash); err != nil {
			return err
		}

		if len(snapshot.Parents) > MaxProjectionEdges-edges {
			return fmt.Errorf("%w: exceeds %d parent edges; select a narrower ref", domain.ErrMemoryProjectionLimit, MaxProjectionEdges)
		}
		edges += len(snapshot.Parents)
		if len(snapshot.GraftParents) > MaxProjectionEdges-edges {
			return fmt.Errorf("%w: exceeds %d parent edges; select a narrower ref", domain.ErrMemoryProjectionLimit, MaxProjectionEdges)
		}
		edges += len(snapshot.GraftParents)
		state[snapshotID] = 1
		for _, parent := range snapshot.ReachabilityParents() {
			if err := visit(parent); err != nil {
				return err
			}
		}
		state[snapshotID] = 2
		return nil
	}
	if err := visit(root); err != nil {
		return "", err
	}
	fingerprint := newMemoryProjectionFingerprinter(ctx, reader)
	hash, complete := fingerprint.byID(root)
	if !complete {
		return "", fmt.Errorf("%w: incomplete memory lineage", domain.ErrIntegrity)
	}
	return hash, nil
}

func (reader *ProjectionReader) GetSnapshot(ctx context.Context, snapshotID domain.ContentHash) (domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		reader.err = err
		return domain.Snapshot{}, err
	}
	if snapshot, ok := reader.snapshots[snapshotID]; ok {
		return snapshot, nil
	}
	reader.err = fmt.Errorf("%w: missing memory lineage snapshot %s", domain.ErrIntegrity, snapshotID)
	return domain.Snapshot{}, reader.err
}
func (reader *ProjectionReader) GetMemory(ctx context.Context, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := ctx.Err(); err != nil {
		reader.err = err
		return domain.MemoryDigest{}, err
	}
	if digest, ok := reader.memories[hash]; ok {
		return digest, nil
	}
	digest, err := reader.source.GetMemory(ctx, reader.repoID, hash)
	if err == nil {
		actual, hashErr := domain.MemoryDigestHash(digest)
		if hashErr != nil {
			err = hashErr
		} else if actual != hash {
			err = fmt.Errorf("%w: memory object hash mismatch", domain.ErrIntegrity)
		}
	}
	if err == nil {
		raw, marshalErr := json.Marshal(digest)
		if marshalErr != nil {
			err = marshalErr
		} else {
			reader.bytes += len(raw)
			limit := MaxProjectionMemoryBytes
			if reader.objectLimit > 0 && reader.objectLimit < limit {
				limit = reader.objectLimit
			}
			if len(raw) > limit {
				err = fmt.Errorf("%w: object exceeds %d bytes; select a narrower ref", domain.ErrMemoryProjectionLimit, limit)
			} else {
				reader.cacheMemory(hash, digest, len(raw))
			}
		}
	}
	if err != nil {
		reader.err = fmt.Errorf("memory projection object %s: %w", hash, err)
		return domain.MemoryDigest{}, reader.err
	}
	if reader.validated == nil {
		reader.validated = map[domain.ContentHash]bool{}
	}
	reader.validated[hash] = true
	return digest, nil
}

func (reader *ProjectionReader) cacheMemory(hash domain.ContentHash, digest domain.MemoryDigest, weight int) {
	limit := reader.cacheLimit
	if limit == 0 {
		limit = MaxProjectionCacheBytes
	}
	if weight > limit {
		return
	}
	if reader.memories == nil {
		reader.memories = map[domain.ContentHash]domain.MemoryDigest{}
	}
	if reader.weights == nil {
		reader.weights = map[domain.ContentHash]int{}
	}
	for reader.cacheBytes+weight > limit && len(reader.order) > 0 {
		evictedHash := reader.order[0]
		reader.order = reader.order[1:]
		reader.cacheBytes -= reader.weights[evictedHash]
		delete(reader.weights, evictedHash)
		delete(reader.memories, evictedHash)
	}
	reader.memories[hash] = digest
	reader.weights[hash] = weight
	reader.order = append(reader.order, hash)
	reader.cacheBytes += weight
}

func (reader *ProjectionReader) memoryValidated(ctx context.Context, hash domain.ContentHash) bool {
	if err := ctx.Err(); err != nil {
		reader.err = err
		return false
	}
	if reader.validated[hash] {
		return true
	}
	_, err := reader.GetMemory(ctx, hash)
	return err == nil
}

func validateProjectionHashes(hashes ...domain.ContentHash) error {
	for _, hash := range hashes {
		if err := domain.ValidateContentHash(hash); err != nil {
			return err
		}
	}
	return nil
}
