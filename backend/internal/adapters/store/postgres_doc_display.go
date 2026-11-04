//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const docDisplayJobColumns = `substring(repo_id FROM 1 FOR 72),substring(doc_hash FROM 1 FOR 72),
 state,substring(reason FROM 1 FOR 129),attempts,version,created_at,next_attempt,lease_until,
 source_id::text,source_version,substring(range_generation FROM 1 FOR 72),event_index,
 substring(event_hash FROM 1 FOR 72),event_offset,event_length,event_seq,substring(event_role FROM 1 FOR 65),
 substring(profile FROM 1 FOR 65)`

const docDisplayKeySQL = `repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile='tool-result-string-v1'`

func docDisplayKey(target domain.DocDisplayTarget) string {
	switch target.DisplayProfile() {
	case domain.DocDisplayStringProfile:
		return docDisplayKeySQL
	case domain.DocDisplayProjectionProfile:
		return `repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile='public-event-display-v1'`
	default:
		return `repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND FALSE`
	}
}

func scanDocDisplayJob(row pgx.Row) (domain.DocDisplayJob, error) {
	var job domain.DocDisplayJob
	var profile string
	var lease *time.Time
	if err := row.Scan(&job.RepoID, &job.DocHash, &job.State, &job.Reason, &job.Attempts, &job.Version,
		&job.CreatedAt, &job.NextAttempt, &lease, &job.Target.Binding.SourceID,
		&job.Target.Binding.SourceVersion, &job.Target.Binding.RangeGeneration, &job.Target.Event.Index,
		&job.Target.Event.Hash, &job.Target.Event.Offset, &job.Target.Event.Length, &job.Target.Event.Seq,
		&job.Target.Event.Role, &profile); err != nil {
		return domain.DocDisplayJob{}, mapNoRows(err)
	}
	job.Target.RepoID, job.Target.DocHash = job.RepoID, job.DocHash
	switch profile {
	case domain.DocDisplayStringProfile:
	case domain.DocDisplayProjectionProfile:
		job.Target.Profile = profile
	default:
		return domain.DocDisplayJob{}, domain.ErrIntegrity
	}
	if lease != nil {
		job.LeaseUntil = *lease
	}
	if job.Validate() != nil || len(job.Reason) > 128 || len(job.Target.Event.Role) > 64 {
		return domain.DocDisplayJob{}, domain.ErrIntegrity
	}
	return job, nil
}

func sameDocDisplayTarget(first, second domain.DocDisplayTarget) bool {
	return first.RepoID == second.RepoID && first.DocHash == second.DocHash && first.Binding == second.Binding && first.DisplayProfile() == second.DisplayProfile() &&
		first.Event.Index == second.Event.Index && first.Event.Hash == second.Event.Hash &&
		first.Event.Offset == second.Event.Offset && first.Event.Length == second.Event.Length
}

func (s *PostgresStore) proveDocDisplayTarget(ctx context.Context, target domain.DocDisplayTarget, lock bool) (domain.DocDisplayTarget, error) {
	entry, err := s.proveDocDisplayEntry(ctx, target, lock)
	if err != nil {
		return domain.DocDisplayTarget{}, err
	}
	target.Event = entry.DocEventIndex
	return target, nil
}

func (storage *PostgresStore) proveDocDisplayEntry(ctx context.Context, target domain.DocDisplayTarget, lock bool) (domain.DocRangeEntry, error) {
	if err := target.Validate(); err != nil {
		return domain.DocRangeEntry{}, err
	}
	if target.DisplayProfile() != domain.DocDisplayStringProfile && target.DisplayProfile() != domain.DocDisplayProjectionProfile {
		return domain.DocRangeEntry{}, domain.ErrValidation
	}
	if lock {
		var version int64
		if err := storage.db(ctx).QueryRow(ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1 FOR SHARE`, target.DocHash).Scan(&version); err != nil {
			return domain.DocRangeEntry{}, mapNoRows(err)
		}
		var present int
		if err := storage.db(ctx).QueryRow(ctx, docRangeJobTypedSourceSQL+` FOR SHARE OF snapshot,owned`, target.RepoID, target.DocHash,
			domain.DocumentIdentityRootV1, domain.DocumentIdentityLegacy).Scan(&present); err != nil {
			return domain.DocRangeEntry{}, mapNoRows(err)
		}
		if err := storage.db(ctx).QueryRow(ctx, `SELECT 1 FROM doc_range_generations_v2 WHERE doc_hash=$1 AND source_version=$2 FOR SHARE`,
			target.DocHash, version).Scan(&present); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.DocRangeEntry{}, domain.ErrDocRangeIndexUnavailable
			}
			return domain.DocRangeEntry{}, err
		}
	}
	var generation domain.DocRangeGeneration
	var binding domain.DocReadGenerationBinding
	var shared *postgresDocRangeGenerationRead
	var err error
	if !lock && storage.InReadOnlyTransaction(ctx) {
		shared, err = storage.openDocRangeGeneration(ctx, target.RepoID, target.DocHash)
		if err == nil {
			generation, binding = shared.generation, shared.binding
		}
	} else {
		generation, binding, err = storage.readRangeGenerationBinding(ctx, target.RepoID, target.DocHash)
	}
	if err != nil {
		return domain.DocRangeEntry{}, err
	}
	if binding != target.Binding {
		return domain.DocRangeEntry{}, domain.ErrConflict
	}
	var identity domain.DocumentIdentity
	if err := storage.db(ctx).QueryRow(ctx, docRangeJobIdentitySQL, target.RepoID, target.DocHash,
		domain.DocumentIdentityRootV1, domain.DocumentIdentityLegacy).Scan(&identity); err != nil {
		return domain.DocRangeEntry{}, mapNoRows(err)
	}
	if identity != generation.Identity() {
		return domain.DocRangeEntry{}, domain.ErrIntegrity
	}
	if int64(target.Event.Index) >= generation.EventCount {
		return domain.DocRangeEntry{}, domain.ErrConflict
	}
	load := func(ctx context.Context, hash domain.ContentHash, limit int) ([]byte, error) {
		if shared != nil {
			return shared.loadObject(ctx, hash, limit)
		}
		var raw []byte
		err := storage.db(ctx).QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $3) FROM doc_range_objects_v2 WHERE doc_hash=$1 AND hash=$2`,
			target.DocHash, hash, limit+1).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrIntegrity
		}
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 || len(raw) > limit || domain.HashContent(raw) != hash {
			return nil, domain.ErrIntegrity
		}
		return raw, ctx.Err()
	}
	position := target.Event.Index / domain.DocRangePageEvents
	hash, err := domain.ResolveDocRangeLeaf(ctx, generation.EventPages, position, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		return load(ctx, hash, domain.MaxDocRangeDirectoryBytes)
	})
	if err != nil {
		return domain.DocRangeEntry{}, err
	}
	raw, err := load(ctx, hash, domain.MaxDocRangePageBytes)
	if err != nil {
		return domain.DocRangeEntry{}, err
	}
	entries, err := generation.DecodePage(position, hash, raw)
	if err != nil {
		return domain.DocRangeEntry{}, err
	}
	entry := entries[target.Event.Index%domain.DocRangePageEvents]
	if entry.Index != target.Event.Index || entry.Hash != target.Event.Hash ||
		entry.Offset != target.Event.Offset || entry.Length != target.Event.Length {
		return domain.DocRangeEntry{}, domain.ErrConflict
	}
	if target.DisplayProfile() == domain.DocDisplayStringProfile && entry.Kind != domain.EventToolResult ||
		target.DisplayProfile() == domain.DocDisplayProjectionProfile && entry.Kind != domain.EventReasoning && entry.Kind != domain.EventMessage && entry.Kind != domain.EventToolCall && entry.Kind != domain.EventToolResult {
		return domain.DocRangeEntry{}, domain.ErrConflict
	}
	return entry, ctx.Err()
}

func (s *PostgresStore) docDisplayWrite(ctx context.Context, job domain.DocDisplayJob, completeAt time.Time, fn func(context.Context, pgx.Tx) error) error {
	return s.docDisplayWriteEntry(ctx, job, completeAt, func(bound context.Context, transaction pgx.Tx, _ domain.DocRangeEntry) error {
		return fn(bound, transaction)
	})
}

func docCatalogWriterError(err error) error {
	var failure *pgconn.PgError
	if errors.As(err, &failure) && failure.Code == "55000" {
		switch failure.Message {
		case "document catalog writer claim is not live":
			return fmt.Errorf("%w: %w", domain.ErrConflict, err)
		case "document catalog source incarnation changed", "document catalog epoch changed":
			return fmt.Errorf("%w: %w", domain.ErrDocDisplayIncarnationChanged, err)
		}
	}
	return err
}

func (s *PostgresStore) docDisplayWriteEntry(ctx context.Context, job domain.DocDisplayJob, completeAt time.Time, fn func(context.Context, pgx.Tx, domain.DocRangeEntry) error) (failure error) {
	defer func() { failure = docCatalogWriterError(failure) }()
	if err := job.Validate(); err != nil {
		return err
	}
	if job.State != "running" {
		return domain.ErrValidation
	}
	transaction, err := s.beginDocRangeJob(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: s, repo: job.RepoID})
	entry, err := s.proveDocDisplayEntry(bound, job.Target, true)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
			return domain.ErrConflict
		}
		return err
	}
	if err := lockDocDisplayJob(bound, transaction, job); err != nil {
		return err
	}
	if _, err := transaction.Exec(bound, `SELECT cxthub_bind_doc_catalog_control($1,$2,$3,$4,$5)`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version); err != nil {
		return err
	}
	if err := fn(bound, transaction, entry); err != nil {
		return err
	}
	if err := checkDocDisplayLive(bound, transaction, job); err != nil {
		return err
	}
	if !completeAt.IsZero() {
		tag, err := transaction.Exec(ctx, `UPDATE doc_display_jobs_v1 SET state='completed',reason='',lease_until=NULL
 WHERE `+docDisplayKey(job.Target)+` AND version=$4 AND state='running' AND lease_until>$5 AND lease_until>clock_timestamp()`,
			job.RepoID, job.DocHash, job.Target.Event.Index, job.Version, completeAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	}
	return transaction.Commit(ctx)
}

func lockDocDisplayJob(ctx context.Context, transaction pgx.Tx, job domain.DocDisplayJob) error {
	stored, err := scanDocDisplayJob(transaction.QueryRow(ctx, `SELECT `+docDisplayJobColumns+` FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target)+` FOR UPDATE`,
		job.RepoID, job.DocHash, job.Target.Event.Index))
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrConflict
	}
	if err != nil {
		return err
	}
	if !sameDocDisplayTarget(job.Target, stored.Target) || stored.Version != job.Version {
		return domain.ErrConflict
	}
	return checkDocDisplayLive(ctx, transaction, job)
}

func checkDocDisplayLive(ctx context.Context, transaction pgx.Tx, job domain.DocDisplayJob) error {
	var live bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target)+`
 AND version=$4 AND state='running' AND lease_until>clock_timestamp())`, job.RepoID, job.DocHash, job.Target.Event.Index, job.Version).Scan(&live); err != nil {
		return err
	}
	if !live {
		return domain.ErrConflict
	}
	return nil
}

func (s *PostgresStore) readDocDisplaySource(ctx context.Context, target domain.DocDisplayTarget, lock bool) (*domain.DocDisplayStringSource, error) {
	var hash domain.ContentHash
	var raw []byte
	statement := `SELECT substring(source_hash FROM 1 FOR 72),substring(bytes FROM 1 FOR $3)
 FROM doc_display_sources_v1 WHERE event_hash=$1 AND profile=$2`
	if lock {
		statement += ` FOR NO KEY UPDATE`
	}
	if err := s.db(ctx).QueryRow(ctx, statement, target.Event.Hash, domain.DocDisplayStringProfile, domain.MaxDocDisplaySourceBytes+1).Scan(&hash, &raw); err != nil {
		return nil, mapNoRows(err)
	}
	source, err := domain.DecodeDocDisplayStringSource(hash, raw)
	if err != nil {
		return nil, err
	}
	if source.EventHash != target.Event.Hash || source.EventBytes != target.Event.Length {
		return nil, domain.ErrIntegrity
	}
	return &source, nil
}

func (s *PostgresStore) readDocDisplayPreparation(ctx context.Context, job domain.DocDisplayJob) (domain.DocDisplayPreparation, error) {
	if job.Target.DisplayProfile() != domain.DocDisplayStringProfile {
		return domain.DocDisplayPreparation{}, domain.ErrValidation
	}
	var sourceHash, checkpointHash, discoveryHash *domain.ContentHash
	var raw, discoveryRaw []byte
	if err := s.db(ctx).QueryRow(ctx, `SELECT substring(source_hash FROM 1 FOR 72),substring(checkpoint_hash FROM 1 FOR 72),
 substring(checkpoint FROM 1 FOR $4),substring(discovery_hash FROM 1 FOR 72),substring(discovery FROM 1 FOR $5)
 FROM doc_display_jobs_v1 WHERE `+docDisplayKeySQL,
		job.RepoID, job.DocHash, job.Target.Event.Index, domain.MaxDocDisplayStateBytes+1,
		domain.MaxDocFieldDiscoveryStateBytes+1).Scan(&sourceHash, &checkpointHash, &raw, &discoveryHash, &discoveryRaw); err != nil {
		return domain.DocDisplayPreparation{}, mapNoRows(err)
	}
	var result domain.DocDisplayPreparation
	rendering := sourceHash != nil || checkpointHash != nil || raw != nil
	discovering := discoveryHash != nil || discoveryRaw != nil
	if rendering && (sourceHash == nil || checkpointHash == nil || raw == nil || discovering) || discovering && (discoveryHash == nil || discoveryRaw == nil) {
		return domain.DocDisplayPreparation{}, domain.ErrIntegrity
	}
	if discovering {
		state, err := domain.DecodeDocFieldDiscoveryState(*discoveryHash, discoveryRaw)
		if err != nil || state.EventHash != job.Target.Event.Hash || state.EventBytes != job.Target.Event.Length || state.Next == 0 || state.Done {
			return domain.DocDisplayPreparation{}, domain.ErrIntegrity
		}
		result.Discovery, result.DiscoveryCheckpoint = &state, *discoveryHash
	}
	source, err := s.readDocDisplaySource(ctx, job.Target, true)
	if errors.Is(err, domain.ErrNotFound) && !rendering {
		return result, nil
	}
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.DocDisplayPreparation{}, domain.ErrIntegrity
		}
		return domain.DocDisplayPreparation{}, err
	}
	if !rendering {
		result.Source = source
		return result, nil
	}
	hash, err := source.Hash()
	if err != nil || sourceHash == nil || *sourceHash != hash || checkpointHash == nil {
		return domain.DocDisplayPreparation{}, domain.ErrIntegrity
	}
	state, err := domain.DecodeDocDisplayStringState(*source, *checkpointHash, raw)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	if err := domain.ValidateDocDisplayStringCheckpoint(ctx, *source, state, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		return s.loadDocDisplayObject(ctx, state.Source, hash)
	}); err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	return domain.DocDisplayPreparation{Source: source, State: state, Checkpoint: *checkpointHash}, nil
}

func (s *PostgresStore) LoadDocDisplayJob(ctx context.Context, job domain.DocDisplayJob) (domain.DocDisplayPreparation, error) {
	var result domain.DocDisplayPreparation
	err := s.docDisplayWrite(ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
		var err error
		result, err = s.readDocDisplayPreparation(bound, job)
		if err == nil {
			result, err = s.reuseDocDisplayString(bound, transaction, job, result)
		}
		return err
	})
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	return result, nil
}

var _ outbound.DocDisplayStore = (*PostgresStore)(nil)
