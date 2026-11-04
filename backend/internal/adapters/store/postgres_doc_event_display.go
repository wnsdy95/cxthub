//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (storage *PostgresStore) readDocEventDisplayPreparation(ctx context.Context, job domain.DocDisplayJob) (domain.DocEventDisplayPreparation, domain.DocDisplayCatalogUse, error) {
	use := domain.DocDisplayCatalogUse{}
	if job.Target.DisplayProfile() != domain.DocDisplayProjectionProfile {
		return domain.DocEventDisplayPreparation{}, use, domain.ErrValidation
	}
	var hash, sourceHash *domain.ContentHash
	var catalogEpoch *string
	var raw []byte
	if err := storage.db(ctx).QueryRow(ctx, `SELECT substring(preparation_hash FROM 1 FOR 72),substring(preparation FROM 1 FOR $4),
 substring(source_hash FROM 1 FOR 72),catalog_epoch::text FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target),
		job.RepoID, job.DocHash, job.Target.Event.Index, domain.MaxDocEventDisplayPreparationBytes+1).Scan(&hash, &raw, &sourceHash, &catalogEpoch); err != nil {
		return domain.DocEventDisplayPreparation{}, use, mapNoRows(err)
	}
	if hash == nil && raw == nil && sourceHash == nil && catalogEpoch == nil {
		return domain.DocEventDisplayPreparation{}, use, domain.ErrNotFound
	}
	if hash == nil || raw == nil || sourceHash == nil {
		return domain.DocEventDisplayPreparation{}, use, domain.ErrIntegrity
	}
	state, err := domain.DecodeDocEventDisplayPreparation(*hash, raw)
	if err != nil || state.Source.EventHash != job.Target.Event.Hash || state.Source.EventBytes != job.Target.Event.Length {
		return domain.DocEventDisplayPreparation{}, use, domain.ErrIntegrity
	}
	expected, err := state.Source.Hash()
	if err != nil || *sourceHash != expected {
		return domain.DocEventDisplayPreparation{}, use, domain.ErrIntegrity
	}
	encoded, err := state.Source.Encode()
	if err != nil {
		return domain.DocEventDisplayPreparation{}, use, err
	}
	var agrees, pinned bool
	var generation, currentEpoch, catalogState *string
	private := state.Phase != "completed" && storage.InReadOnlyTransaction(ctx) && (state.ToolTree != nil || state.ToolKeys != nil || state.ToolOrders != nil)
	if err := storage.db(ctx).QueryRow(ctx, `WITH owned AS MATERIALIZED (SELECT EXISTS(SELECT 1 FROM doc_display_sources_v1
 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3 AND bytes=$4) AS agrees,
 (SELECT generation::text FROM doc_display_catalog_admissions_v1 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3) AS generation,
 (SELECT catalog_epoch::text FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=$3) AS catalog_epoch,
 (SELECT state FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=$3) AS state)
 SELECT agrees,generation,catalog_epoch,state,CASE WHEN agrees AND generation IS NOT NULL
 AND generation<>'00000000-0000-0000-0000-000000000000' AND catalog_epoch=$7 AND state='live' AND $5
 THEN pg_try_advisory_xact_lock_shared(hashtextextended($6,0)) ELSE true END FROM owned`, job.Target.Event.Hash,
		domain.DocDisplayProjectionProfile, expected, encoded, private, docDisplayCatalogUseKey(job.Target.Event.Hash, domain.DocDisplayProjectionProfile), catalogEpoch).
		Scan(&agrees, &generation, &currentEpoch, &catalogState, &pinned); err != nil {
		return domain.DocEventDisplayPreparation{}, use, err
	}
	if !agrees || generation == nil || catalogEpoch == nil || currentEpoch == nil || catalogState == nil {
		return domain.DocEventDisplayPreparation{}, use, domain.ErrIntegrity
	}
	use = domain.DocDisplayCatalogUse{Source: expected, Generation: *generation, Epoch: *catalogEpoch}
	if err := use.Validate(); err != nil {
		return domain.DocEventDisplayPreparation{}, domain.DocDisplayCatalogUse{}, err
	}
	if state.Phase != "completed" && *catalogEpoch != *currentEpoch {
		return state, use, domain.ErrDocDisplayIncarnationChanged
	}
	if state.Phase != "completed" && *catalogState != "live" {
		return state, use, domain.ErrResourceOverloaded
	}
	if !pinned {
		return domain.DocEventDisplayPreparation{}, domain.DocDisplayCatalogUse{}, domain.ErrResourceOverloaded
	}
	return state, use, ctx.Err()
}

func (storage *PostgresStore) LoadDocEventDisplay(ctx context.Context, job domain.DocDisplayJob) (domain.DocEventDisplayPreparation, error) {
	if job.Target.DisplayProfile() != domain.DocDisplayProjectionProfile {
		return domain.DocEventDisplayPreparation{}, domain.ErrValidation
	}
	var result domain.DocEventDisplayPreparation
	err := storage.docDisplayWriteEntry(ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx, entry domain.DocRangeEntry) error {
		initial, err := domain.NewDocEventDisplayPreparation(entry)
		if err != nil {
			return err
		}
		var priorUse domain.DocDisplayCatalogUse
		result, priorUse, err = storage.readDocEventDisplayPreparation(bound, job)
		hasPrior := err == nil
		replacePrior := errors.Is(err, domain.ErrDocDisplayIncarnationChanged) || errors.Is(err, domain.ErrResourceOverloaded)
		var priorHash, priorSource, priorEpoch any
		if hasPrior {
			if result.Source != initial.Source {
				return domain.ErrIntegrity
			}
			if result.Phase == "completed" {
				return nil
			}
			encoded, err := result.Encode()
			if err != nil {
				return err
			}
			priorHash = domain.HashContent(encoded)
			priorEpoch = priorUse.Epoch
			priorSource, err = result.Source.Hash()
			if err != nil {
				return err
			}
		} else if !replacePrior && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		raw, err := initial.Source.Encode()
		if err != nil {
			return err
		}
		sourceHash := domain.HashContent(raw)
		if replacePrior {
			var storedHash, storedSource *domain.ContentHash
			var storedEpoch *string
			if err := transaction.QueryRow(bound, `SELECT preparation_hash,source_hash,catalog_epoch::text FROM doc_display_jobs_v1
 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index).Scan(&storedHash, &storedSource, &storedEpoch); err != nil {
				return err
			}
			if storedHash == nil || storedSource == nil || storedEpoch == nil || *storedSource != sourceHash || domain.ValidateDocDisplayCatalogEpoch(*storedEpoch) != nil {
				return domain.ErrIntegrity
			}
			priorHash, priorSource, priorEpoch = *storedHash, *storedSource, *storedEpoch
		}
		if !hasPrior && !replacePrior {
			if _, err := transaction.Exec(bound, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, initial.Source.EventHash, initial.Source.Profile, sourceHash, raw); err != nil {
				return err
			}
		}
		var agrees bool
		if err := transaction.QueryRow(bound, `SELECT bytes=$4 FROM doc_display_sources_v1
 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3 FOR NO KEY UPDATE`, initial.Source.EventHash,
			initial.Source.Profile, sourceHash, raw).Scan(&agrees); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrIntegrity
			}
			return err
		}
		if !agrees {
			return domain.ErrIntegrity
		}
		epoch, lifecycle, err := readDocDisplayCatalogState(bound, transaction, sourceHash)
		if err != nil {
			return err
		}
		shared, sharedEpoch, reusable, err := readCompletedDocEventDisplay(bound, transaction, initial.Source, sourceHash)
		if err != nil {
			return err
		}
		if reusable {
			initial = shared
			epoch = sharedEpoch
		} else if lifecycle == "retired" {
			encoded, err := initial.Encode()
			if err != nil {
				return err
			}
			var fresh string
			if err := transaction.QueryRow(bound, `SELECT cxthub_restart_doc_catalog($1,$2::uuid,$3,$4)::text`, sourceHash, epoch,
				domain.HashContent(encoded), encoded).Scan(&fresh); err != nil {
				return err
			}
			if domain.ValidateDocDisplayCatalogEpoch(fresh) != nil || fresh == epoch {
				return domain.ErrIntegrity
			}
			result = initial
			return nil
		} else if lifecycle != "live" {
			return domain.ErrResourceOverloaded
		} else if hasPrior {
			return nil
		}
		encoded, err := initial.Encode()
		if err != nil {
			return err
		}
		tag, err := transaction.Exec(bound, `UPDATE doc_display_jobs_v1 SET source_hash=$4,preparation_hash=$5,preparation=$6,catalog_epoch=$7::uuid
 WHERE `+docDisplayKey(job.Target)+` AND preparation_hash IS NOT DISTINCT FROM $9 AND source_hash IS NOT DISTINCT FROM $10
 AND catalog_epoch IS NOT DISTINCT FROM $11::uuid AND state='running' AND version=$8 AND lease_until>clock_timestamp()`, job.RepoID, job.DocHash,
			job.Target.Event.Index, sourceHash, domain.HashContent(encoded), encoded, epoch, job.Version, priorHash, priorSource, priorEpoch)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrConflict
		}
		result = initial
		return nil
	})
	if err != nil {
		return domain.DocEventDisplayPreparation{}, err
	}
	return result, nil
}

func (storage *PostgresStore) SaveDocEventDisplay(ctx context.Context, job domain.DocDisplayJob, step domain.DocEventDisplayStep) (domain.DocEventDisplayPreparation, error) {
	if job.Target.DisplayProfile() != domain.DocDisplayProjectionProfile || domain.ValidateContentHash(step.Prior()) != nil {
		return domain.DocEventDisplayPreparation{}, domain.ErrValidation
	}
	result := step.Preparation()
	raw, err := result.Encode()
	if err != nil {
		return domain.DocEventDisplayPreparation{}, err
	}
	objects := step.Objects()
	if len(objects) == 0 || len(objects) > domain.MaxDocEventDisplayStepObjects {
		return domain.DocEventDisplayPreparation{}, domain.ErrIntegrity
	}
	use, hasUse := step.CatalogUse()
	if !hasUse || use.Validate() != nil {
		return domain.DocEventDisplayPreparation{}, domain.ErrConflict
	}
	err = storage.docDisplayWrite(ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
		prior, currentUse, err := storage.readDocEventDisplayPreparation(bound, job)
		if err != nil && !errors.Is(err, domain.ErrDocDisplayIncarnationChanged) && !errors.Is(err, domain.ErrResourceOverloaded) {
			return err
		}
		encoded, encodeErr := prior.Encode()
		if encodeErr != nil || domain.HashContent(encoded) != step.Prior() || prior.Source != result.Source {
			return domain.ErrConflict
		}
		if err != nil {
			return err
		}
		if currentUse != use {
			return domain.ErrDocDisplayIncarnationChanged
		}
		if _, err := transaction.Exec(bound, `SELECT cxthub_bind_doc_catalog_calculation($1,$2,$3,$4,$5,$6,$7::uuid,$8::uuid,$9)`,
			job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version, use.Source, use.Generation, use.Epoch, step.Prior()); err != nil {
			return err
		}
		sourceHash, err := result.Source.Hash()
		if err != nil {
			return err
		}
		if err := putDocDisplayObjects(bound, transaction, sourceHash, objects); err != nil {
			return err
		}
		if err := storage.saveDocToolTreeScan(bound, transaction, sourceHash, prior, result, step); err != nil {
			return err
		}
		if err := storage.saveDocToolKeyMap(bound, transaction, sourceHash, prior, result, step); err != nil {
			return err
		}
		if err := storage.saveDocToolOrders(bound, transaction, sourceHash, prior, result, step); err != nil {
			return err
		}
		tag, err := transaction.Exec(bound, `UPDATE doc_display_jobs_v1 SET preparation_hash=$4,preparation=$5
 WHERE `+docDisplayKey(job.Target)+` AND preparation_hash=$6 AND source_hash=$7 AND version=$8
 AND state='running' AND lease_until>clock_timestamp()`, job.RepoID, job.DocHash, job.Target.Event.Index,
			domain.HashContent(raw), raw, step.Prior(), sourceHash, job.Version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrConflict
		}
		return nil
	})
	if err != nil {
		return domain.DocEventDisplayPreparation{}, err
	}
	return result, nil
}

func (storage *PostgresStore) CompleteDocEventDisplay(ctx context.Context, job domain.DocDisplayJob, checkpoint domain.ContentHash, now time.Time) error {
	if job.Target.DisplayProfile() != domain.DocDisplayProjectionProfile || domain.ValidateContentHash(checkpoint) != nil || now.IsZero() {
		return domain.ErrValidation
	}
	return storage.docDisplayWrite(ctx, job, now, func(bound context.Context, transaction pgx.Tx) error {
		state, _, err := storage.readDocEventDisplayPreparation(bound, job)
		if err != nil {
			return err
		}
		encoded, err := state.Encode()
		if err != nil || domain.HashContent(encoded) != checkpoint {
			return domain.ErrConflict
		}
		sourceHash, err := state.Source.Hash()
		if err != nil {
			return err
		}
		generation, err := domain.ValidateDocEventDisplayPublication(bound, state, func(read context.Context, hash domain.ContentHash) ([]byte, error) {
			return storage.loadDocDisplayObject(read, sourceHash, hash)
		})
		if err != nil {
			return err
		}
		raw, err := generation.Encode()
		if err != nil || domain.HashContent(raw) != state.Generation {
			return domain.ErrIntegrity
		}
		tag, err := transaction.Exec(bound, `INSERT INTO doc_display_generations_v1(event_hash,profile,source_hash,generation_hash,bytes)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, state.Source.EventHash, state.Source.Profile, sourceHash, state.Generation, raw)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var agrees bool
			if err := transaction.QueryRow(bound, `SELECT EXISTS(SELECT 1 FROM doc_display_generations_v1
 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3 AND generation_hash=$4 AND bytes=$5)`,
				state.Source.EventHash, state.Source.Profile, sourceHash, state.Generation, raw).Scan(&agrees); err != nil {
				return err
			}
			if !agrees {
				return domain.ErrIntegrity
			}
		}
		return nil
	})
}

type postgresDocEventDisplayWork struct {
	storage      *PostgresStore
	owner        *repositoryTx
	source       domain.ContentHash
	tree         *domain.DocToolTreeSource
	keys         *domain.DocToolKeyMapSource
	orders       *domain.DocToolOrdersSource
	checkpoint   domain.ContentHash
	use          domain.DocDisplayCatalogUse
	orderCatalog *atomic.Pointer[docToolOrderCatalogRead]
}

func (reader *postgresDocEventDisplayWork) checkTransaction(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader == nil || reader.storage == nil || reader.owner == nil || reader.owner.Tx == nil ||
		ctx.Value(repositoryTxKey{}) != reader.owner || !reader.storage.InReadOnlyTransaction(ctx) {
		return domain.ErrConflict
	}
	if err := reader.owner.SendBatch(ctx, &pgx.Batch{}).Close(); err != nil {
		return err
	}
	if reader.owner.readLive != nil {
		return reader.owner.readLive()
	}
	return reader.storage.requireDocDisplayRead(ctx)
}

func (reader *postgresDocEventDisplayWork) PrepareStep(ctx context.Context, prior domain.DocEventDisplayPreparation, loadRaw func(context.Context, int, int) ([]byte, error), tools ...domain.DocToolPreparationReader) (domain.DocEventDisplayStep, error) {
	if err := reader.checkTransaction(ctx); err != nil {
		return domain.DocEventDisplayStep{}, err
	}
	if reader.use.Validate() != nil || reader.use.Source != reader.source {
		return domain.DocEventDisplayStep{}, domain.ErrIntegrity
	}
	encoded, err := prior.Encode()
	if err != nil || domain.HashContent(encoded) != reader.checkpoint {
		return domain.DocEventDisplayStep{}, domain.ErrConflict
	}
	step, err := domain.StepDocEventDisplay(ctx, prior, loadRaw, reader.LoadObject, tools...)
	if err != nil {
		return domain.DocEventDisplayStep{}, err
	}
	if err := reader.checkTransaction(ctx); err != nil {
		return domain.DocEventDisplayStep{}, err
	}
	return step.WithCatalogUse(reader.use)
}

func (reader *postgresDocEventDisplayWork) LoadObject(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil || reader.storage == nil || reader.owner == nil || reader.owner.Tx == nil ||
		ctx.Value(repositoryTxKey{}) != reader.owner || !reader.storage.InReadOnlyTransaction(ctx) {
		return nil, domain.ErrConflict
	}
	return reader.storage.loadDocDisplayObject(ctx, reader.source, hash)
}

func (storage *PostgresStore) OpenDocEventDisplayWork(ctx context.Context, job domain.DocDisplayJob, checkpoint domain.ContentHash) (outbound.DocEventDisplayWorkReader, error) {
	if job.Validate() != nil || job.State != "running" || job.Target.DisplayProfile() != domain.DocDisplayProjectionProfile || domain.ValidateContentHash(checkpoint) != nil {
		return nil, domain.ErrValidation
	}
	if err := storage.requireDocDisplayRead(ctx); err != nil {
		return nil, err
	}
	if _, err := storage.proveDocDisplayTarget(ctx, job.Target, false); err != nil {
		return nil, err
	}
	var live bool
	if err := storage.db(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_display_jobs_v1
 WHERE `+docDisplayKey(job.Target)+` AND state='running' AND version=$4 AND preparation_hash=$5
 AND lease_until>clock_timestamp())`, job.RepoID, job.DocHash, job.Target.Event.Index, job.Version, checkpoint).Scan(&live); err != nil {
		return nil, err
	}
	if !live {
		return nil, domain.ErrConflict
	}
	state, use, err := storage.readDocEventDisplayPreparation(ctx, job)
	if err != nil {
		return nil, err
	}
	source, err := state.Source.Hash()
	if err != nil {
		return nil, err
	}
	owner, _ := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	return &postgresDocEventDisplayWork{
		storage: storage, owner: owner, source: source, tree: state.ToolTree, keys: state.ToolKeys, orders: state.ToolOrders,
		checkpoint: checkpoint, use: use, orderCatalog: new(atomic.Pointer[docToolOrderCatalogRead]),
	}, nil
}

func equalDocEventDisplaySource(source domain.DocEventDisplaySource, raw []byte, hash domain.ContentHash) bool {
	expected, err := source.Encode()
	return err == nil && domain.HashContent(expected) == hash && bytes.Equal(expected, raw)
}

var _ outbound.DocEventDisplayStore = (*PostgresStore)(nil)
