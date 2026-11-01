//go:build postgres

package store

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docRangeBoundaryScopeSQL = docRangeJobOwnedSourceSQL + ` AND snapshot.doc_identity IN ($3,$5) AND EXISTS(
 SELECT 1 FROM snapshots base_snapshot
 JOIN repo_blobs base_owned ON base_owned.repo_id=base_snapshot.repo_id AND base_owned.kind='doc' AND base_owned.hash=base_snapshot.doc_hash
 WHERE base_snapshot.repo_id=$1 AND base_snapshot.id=$4 AND base_snapshot.doc_hash=$4 AND base_snapshot.doc_identity IN ($3,$5))`

const docRangeBoundaryReceiptSQL = `SELECT ` + docRangeJobColumns + ` FROM doc_range_jobs_v1 job
 WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.base_hash=$4 AND EXISTS(` + docRangeBoundaryScopeSQL + `)`

const docRangeBoundaryCheckpointSQL = `SELECT doc_source_version,base_source_version,doc_generation_hash,base_generation_hash,job_version,events,complete
 FROM doc_range_boundaries_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3 AND algorithm=$4`

type docRangeBoundarySource struct {
	hash         domain.ContentHash
	version      int64
	descriptor   domain.ContentHash
	generationID string
	generation   domain.DocRangeGeneration
}

func (source docRangeBoundarySource) sameGeneration(current docRangeBoundarySource) bool {
	return source.hash == current.hash && source.version == current.version && source.descriptor == current.descriptor &&
		source.generationID == current.generationID && source.generation.Identity() == current.generation.Identity()
}

type docRangeBoundaryCheckpoint struct {
	docVersion     int64
	baseVersion    int64
	docDescriptor  domain.ContentHash
	baseDescriptor domain.ContentHash
	jobVersion     int64
	events         int
	complete       bool
}

func docRangeBoundaryHashes(doc, base domain.ContentHash) []domain.ContentHash {
	hashes := []domain.ContentHash{doc}
	if base != doc {
		hashes = append(hashes, base)
	}
	slices.Sort(hashes)
	return hashes
}

func (s *PostgresStore) readDocRangeBoundarySources(ctx context.Context, repo, doc, base domain.ContentHash) ([2]docRangeBoundarySource, error) {
	var sources [2]docRangeBoundarySource
	hashes := docRangeBoundaryHashes(doc, base)
	rows, err := s.db(ctx).Query(ctx, `SELECT root.hash,root.doc_range_source_version,snapshot.doc_identity,generation.generation_id::text,
 substring(generation.generation_hash FROM 1 FOR $5),substring(generation.descriptor FROM 1 FOR $4)
 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.kind='doc' AND owned.hash=snapshot.doc_hash
 JOIN blobs root ON root.hash=owned.hash
 LEFT JOIN doc_range_generations_v2 generation ON generation.doc_hash=root.hash AND generation.source_version=root.doc_range_source_version
 WHERE snapshot.repo_id=$1 AND snapshot.id=ANY($2::text[]) AND snapshot.doc_hash=snapshot.id AND snapshot.doc_identity IN ($3,$6)
 ORDER BY root.hash`, repo, hashes, domain.DocumentIdentityRootV1, domain.MaxDocRangeGenerationBytes+1, len(doc)+1, domain.DocumentIdentityLegacy)
	if err != nil {
		return sources, err
	}
	defer rows.Close()
	type storedSource struct {
		version      int64
		identity     domain.DocumentIdentity
		generationID *string
		descriptor   *domain.ContentHash
		raw          []byte
	}
	stored := make(map[domain.ContentHash]storedSource, len(hashes))
	for rows.Next() {
		var hash domain.ContentHash
		var source storedSource
		if err := rows.Scan(&hash, &source.version, &source.identity, &source.generationID, &source.descriptor, &source.raw); err != nil {
			return sources, err
		}
		stored[hash] = source
	}
	if err := rows.Err(); err != nil {
		return sources, err
	}
	if len(stored) != len(hashes) {
		return sources, domain.ErrNotFound
	}
	for position, hash := range []domain.ContentHash{doc, base} {
		source, exists := stored[hash]
		if !exists {
			return sources, domain.ErrNotFound
		}
		if source.descriptor == nil {
			return sources, domain.ErrDocRangeIndexUnavailable
		}
		if source.version < 0 {
			return sources, domain.ErrIntegrity
		}
		generation, err := domain.DecodeDocRangeGeneration(hash, *source.descriptor, source.raw)
		if err != nil {
			return sources, err
		}
		if generation.Identity() != source.identity {
			return sources, domain.ErrIntegrity
		}
		if source.generationID == nil {
			return sources, domain.ErrIntegrity
		}
		binding := domain.DocReadGenerationBinding{SourceID: *source.generationID, SourceVersion: source.version, RangeGeneration: *source.descriptor}
		if err := binding.Validate(); err != nil {
			return sources, err
		}
		sources[position] = docRangeBoundarySource{hash: hash, version: source.version, descriptor: *source.descriptor, generationID: *source.generationID, generation: generation}
	}
	return sources, ctx.Err()
}

func readDocRangeBoundaryCheckpoint(ctx context.Context, database pgDatabase, repo, doc, base domain.ContentHash, locked bool) (docRangeBoundaryCheckpoint, bool, error) {
	var checkpoint docRangeBoundaryCheckpoint
	statement := docRangeBoundaryCheckpointSQL
	if locked {
		statement += ` FOR UPDATE`
	}
	err := database.QueryRow(ctx, statement, repo, doc, base, domain.DocRangePrefixAlgorithm).Scan(
		&checkpoint.docVersion, &checkpoint.baseVersion, &checkpoint.docDescriptor, &checkpoint.baseDescriptor,
		&checkpoint.jobVersion, &checkpoint.events, &checkpoint.complete)
	if errors.Is(err, pgx.ErrNoRows) {
		return checkpoint, false, nil
	}
	if err != nil {
		return checkpoint, false, err
	}
	if checkpoint.docVersion < 0 || checkpoint.baseVersion < 0 || checkpoint.jobVersion < 0 ||
		domain.ValidateContentHash(checkpoint.docDescriptor) != nil || domain.ValidateContentHash(checkpoint.baseDescriptor) != nil {
		return checkpoint, false, domain.ErrIntegrity
	}
	return checkpoint, true, ctx.Err()
}

func (checkpoint docRangeBoundaryCheckpoint) matches(sources [2]docRangeBoundarySource) bool {
	return checkpoint.docVersion == sources[0].version && checkpoint.baseVersion == sources[1].version &&
		checkpoint.docDescriptor == sources[0].descriptor && checkpoint.baseDescriptor == sources[1].descriptor
}

func (checkpoint docRangeBoundaryCheckpoint) validate(sources [2]docRangeBoundarySource) error {
	total := int(min(sources[0].generation.EventCount, sources[1].generation.EventCount))
	if checkpoint.events < 0 || checkpoint.events > total ||
		!checkpoint.complete && (checkpoint.events == total || checkpoint.events%domain.DocRangePageEvents != 0) {
		return domain.ErrIntegrity
	}
	return nil
}

func (s *PostgresStore) readDocRangeBoundary(ctx context.Context, repo, doc, base domain.ContentHash) (int, error) {
	sources, err := s.readDocRangeBoundarySources(ctx, repo, doc, base)
	if err != nil {
		return 0, err
	}
	if doc == base {
		return int(sources[0].generation.EventCount), ctx.Err()
	}
	checkpoint, exists, err := readDocRangeBoundaryCheckpoint(ctx, s.db(ctx), repo, doc, base, false)
	if err != nil {
		return 0, err
	}
	if !exists || !checkpoint.matches(sources) {
		return 0, domain.ErrDocRangeIndexUnavailable
	}
	if err := checkpoint.validate(sources); err != nil {
		return 0, err
	}
	if !checkpoint.complete {
		return 0, domain.ErrDocRangeIndexUnavailable
	}
	return checkpoint.events, ctx.Err()
}

func (s *PostgresStore) DocRangeBoundary(ctx context.Context, repo, doc, base domain.ContentHash) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !s.InReadOnlyTransaction(ctx) {
		return 0, domain.ErrConflict
	}
	if err := validateHashes(repo, doc, base); err != nil {
		return 0, err
	}
	return s.readDocRangeBoundary(ctx, repo, doc, base)
}

func (s *PostgresStore) docRangeBoundaryReceipt(ctx context.Context, repo, doc, base domain.ContentHash) (domain.DocRangeIndexJob, error) {
	job, err := scanDocRangeJob(s.db(ctx).QueryRow(ctx, docRangeBoundaryReceiptSQL, repo, doc, domain.DocumentIdentityRootV1, base, domain.DocumentIdentityLegacy))
	if err != nil || job.State != "completed" {
		return job, err
	}
	if _, err := s.readDocRangeBoundary(ctx, repo, doc, base); err != nil {
		if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
			return domain.DocRangeIndexJob{}, domain.ErrNotFound
		}
		return domain.DocRangeIndexJob{}, err
	}
	return job, nil
}

var _ outbound.DocRangeBoundaryStore = (*PostgresStore)(nil)
