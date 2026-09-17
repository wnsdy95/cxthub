package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// MemoryProjectionSource is read-only and repository-scoped. Snapshot metadata
// is read in batches; projection never performs a database request per edge.
type MemoryProjectionSource interface {
	ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error)
	GetMemory(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error)
}

type serviceProjectionSource struct {
	outbound.MetadataStore
	outbound.BlobStore
}

func (s *Service) getMemoryProjection(ctx context.Context, repoID, id domain.ContentHash) (domain.MemoryProjection, error) {
	return ProjectMemory(ctx, serviceProjectionSource{s.meta, s.blobs}, repoID, id)
}

const maxProjectionSnapshots = 4096
const maxProjectionMemoryBytes = 64 << 20

// ProjectMemory derives current project knowledge without attaching a new
// memory object. Two metadata reads validate a consistent transitive state;
// immutable blobs are cached only for this request. It works across replicas.
func ProjectMemory(ctx context.Context, source MemoryProjectionSource, repoID, id domain.ContentHash) (domain.MemoryProjection, error) {
	if err := validateHashes(repoID, id); err != nil {
		return domain.MemoryProjection{}, err
	}
	reader := &projectionReader{source: source, repoID: repoID, memories: map[domain.ContentHash]domain.MemoryDigest{}}
	for attempt := 0; attempt < 3; attempt++ {
		before, err := reader.readState(ctx, id)
		if err != nil {
			return domain.MemoryProjection{}, err
		}
		digest, found, complete := memoryProjectionFromDetailed(ctx, reader, id)
		if reader.err != nil {
			return domain.MemoryProjection{}, reader.err
		}
		if !complete {
			return domain.MemoryProjection{}, fmt.Errorf("%w: incomplete memory projection", domain.ErrIntegrity)
		}
		after, err := reader.readState(ctx, id)
		if err != nil {
			return domain.MemoryProjection{}, err
		}
		if before != after {
			continue
		}
		// This is a derived view, not a persisted attachment or a causal revision.
		digest.SnapshotID = id
		digest.PreviousMemoryHash = ""
		digest.GraftCoverage = nil
		return domain.MemoryProjection{Digest: digest, Found: found, StateHash: after}, nil
	}
	return domain.MemoryProjection{}, fmt.Errorf("%w: memory projection changed during read; retry", domain.ErrConflict)
}

type projectionReader struct {
	source    MemoryProjectionSource
	repoID    domain.ContentHash
	snapshots map[domain.ContentHash]domain.Snapshot
	memories  map[domain.ContentHash]domain.MemoryDigest
	bytes     int
	err       error
}

func (r *projectionReader) readState(ctx context.Context, root domain.ContentHash) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	snapshots, err := r.source.ListSnapshots(ctx, r.repoID, "")
	if err != nil {
		return "", err
	}
	r.snapshots = make(map[domain.ContentHash]domain.Snapshot, len(snapshots))
	for _, s := range snapshots {
		r.snapshots[s.ID] = s
	}
	// Validate only this root's closure. Unrelated retained history cannot make
	// a selected branch unreadable, and missing edges/cycles cannot be hidden by
	// a covering digest. Bound recursion before invoking the lineage walker.
	state := map[domain.ContentHash]uint8{}
	var visit func(domain.ContentHash) error
	visit = func(id domain.ContentHash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if state[id] == 2 {
			return nil
		}
		if state[id] == 1 {
			return fmt.Errorf("%w: cycle in memory lineage", domain.ErrIntegrity)
		}
		if len(state) >= maxProjectionSnapshots {
			return fmt.Errorf("memory projection exceeds %d snapshots; select a narrower ref", maxProjectionSnapshots)
		}
		s, ok := r.snapshots[id]
		if !ok {
			return fmt.Errorf("%w: missing memory lineage snapshot %s", domain.ErrIntegrity, id)
		}
		if s.RepoID != r.repoID {
			return fmt.Errorf("%w: memory lineage repository mismatch", domain.ErrIntegrity)
		}
		if err := domain.ValidateContentHash(id); err != nil {
			return err
		}
		if err := domain.ValidateOptionalContentHash(s.MemoryHash); err != nil {
			return err
		}
		state[id] = 1
		for _, p := range s.ReachabilityParents() {
			if err := visit(p); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	if err := visit(root); err != nil {
		return "", err
	}
	fingerprint := newMemoryProjectionFingerprinter(ctx, r)
	hash, complete := fingerprint.byID(root) // includes the root's mutable pointer
	if !complete {
		return "", fmt.Errorf("%w: incomplete memory lineage", domain.ErrIntegrity)
	}
	return hash, nil
}

func (r *projectionReader) GetSnapshot(ctx context.Context, id domain.ContentHash) (domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		r.err = err
		return domain.Snapshot{}, err
	}
	if s, ok := r.snapshots[id]; ok {
		return s, nil
	}
	r.err = fmt.Errorf("%w: missing memory lineage snapshot %s", domain.ErrIntegrity, id)
	return domain.Snapshot{}, r.err
}
func (r *projectionReader) GetMemory(ctx context.Context, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := ctx.Err(); err != nil {
		r.err = err
		return domain.MemoryDigest{}, err
	}
	if d, ok := r.memories[hash]; ok {
		return d, nil
	}
	d, err := r.source.GetMemory(ctx, r.repoID, hash)
	if err == nil {
		actual, hashErr := domain.MemoryDigestHash(d)
		if hashErr != nil {
			err = hashErr
		} else if actual != hash {
			err = fmt.Errorf("%w: memory object hash mismatch", domain.ErrIntegrity)
		}
	}
	if err == nil {
		raw, marshalErr := json.Marshal(d)
		if marshalErr != nil {
			err = marshalErr
		} else {
			r.bytes += len(raw)
			if r.bytes > maxProjectionMemoryBytes {
				err = fmt.Errorf("memory projection exceeds %d bytes; select a narrower ref", maxProjectionMemoryBytes)
			}
		}
	}
	if err != nil {
		r.err = fmt.Errorf("memory projection object %s: %w", hash, err)
		return domain.MemoryDigest{}, r.err
	}
	r.memories[hash] = d
	return d, nil
}
