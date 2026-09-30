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
const maxProjectionEdges = 1 << 18
const maxProjectionMemoryBytes = 64 << 20
const maxProjectionCacheBytes = 8 << 20

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
		digest, found, complete, walkErr := memoryProjectionFromDetailed(ctx, reader, id)
		if reader.err != nil {
			return domain.MemoryProjection{}, reader.err
		}
		if walkErr != nil {
			return domain.MemoryProjection{}, walkErr
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
	source      MemoryProjectionSource
	repoID      domain.ContentHash
	snapshots   map[domain.ContentHash]domain.Snapshot
	memories    map[domain.ContentHash]domain.MemoryDigest
	weights     map[domain.ContentHash]int
	order       []domain.ContentHash
	validated   map[domain.ContentHash]bool
	cacheBytes  int
	cacheLimit  int // zero uses the production capacity; tests may choose less.
	objectLimit int // zero uses the production bound; tests may choose less.
	bytes       int
	err         error
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
	edges := 0
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
			return fmt.Errorf("%w: exceeds %d snapshots; select a narrower ref", domain.ErrMemoryProjectionLimit, maxProjectionSnapshots)
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
		// Charge raw edges before ReachabilityParents or fingerprinting can
		// allocate a deduplicated list or serialize repeated parent records.
		// A covering digest must not bypass this topology bound.
		if len(s.Parents) > maxProjectionEdges-edges {
			return fmt.Errorf("%w: exceeds %d parent edges; select a narrower ref", domain.ErrMemoryProjectionLimit, maxProjectionEdges)
		}
		edges += len(s.Parents)
		if len(s.GraftParents) > maxProjectionEdges-edges {
			return fmt.Errorf("%w: exceeds %d parent edges; select a narrower ref", domain.ErrMemoryProjectionLimit, maxProjectionEdges)
		}
		edges += len(s.GraftParents)
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
			r.bytes += len(raw) // observed read volume, not retained-memory capacity.
			limit := maxProjectionMemoryBytes
			if r.objectLimit > 0 && r.objectLimit < limit {
				limit = r.objectLimit
			}
			if len(raw) > limit {
				err = fmt.Errorf("%w: object exceeds %d bytes; select a narrower ref", domain.ErrMemoryProjectionLimit, limit)
			} else {
				r.cacheMemory(hash, d, len(raw))
			}
		}
	}
	if err != nil {
		r.err = fmt.Errorf("memory projection object %s: %w", hash, err)
		return domain.MemoryDigest{}, r.err
	}
	if r.validated == nil {
		r.validated = map[domain.ContentHash]bool{}
	}
	r.validated[hash] = true
	return d, nil
}

// Cache immutable bodies only within a fixed retained weight. Evicted bodies
// are re-read and hash-validated if their contribution is needed later.
func (r *projectionReader) cacheMemory(hash domain.ContentHash, d domain.MemoryDigest, weight int) {
	limit := r.cacheLimit
	if limit == 0 {
		limit = maxProjectionCacheBytes
	}
	if weight > limit {
		return
	}
	if r.memories == nil {
		r.memories = map[domain.ContentHash]domain.MemoryDigest{}
	}
	if r.weights == nil {
		r.weights = map[domain.ContentHash]int{}
	}
	for r.cacheBytes+weight > limit && len(r.order) > 0 {
		old := r.order[0]
		r.order = r.order[1:]
		r.cacheBytes -= r.weights[old]
		delete(r.weights, old)
		delete(r.memories, old)
	}
	r.memories[hash] = d
	r.weights[hash] = weight
	r.order = append(r.order, hash)
	r.cacheBytes += weight
}

// Reproducibility uses the same request-local validation evidence as the old
// whole-body cache. It does not need to retain or reconstruct that body again.
// A subsequent contribution reload still validates the current bytes.
func (r *projectionReader) memoryValidated(ctx context.Context, hash domain.ContentHash) bool {
	if err := ctx.Err(); err != nil {
		r.err = err
		return false
	}
	if r.validated[hash] {
		return true
	}
	_, err := r.GetMemory(ctx, hash)
	return err == nil
}
