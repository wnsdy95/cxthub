//go:build postgres

package app

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docDisplayIncarnationProbePG struct {
	*store.PostgresStore
	pool          *pgxpool.Pool
	changed       bool
	catalogEpoch  bool
	database      string
	before, after string
}

func (probe *docDisplayIncarnationProbePG) SaveDocEventDisplay(ctx context.Context, job domain.DocDisplayJob, step domain.DocEventDisplayStep) (domain.DocEventDisplayPreparation, error) {
	if !probe.changed {
		use, sealed := step.CatalogUse()
		if !sealed {
			return domain.DocEventDisplayPreparation{}, domain.ErrIntegrity
		}
		transaction, err := probe.pool.Begin(ctx)
		if err != nil {
			return domain.DocEventDisplayPreparation{}, err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			_ = transaction.Rollback(cleanup)
		}()
		var raw []byte
		if err := transaction.QueryRow(ctx, `SELECT source.bytes,admission.generation::text FROM doc_display_sources_v1 source
 JOIN doc_display_catalog_admissions_v1 admission ON admission.event_hash=source.event_hash AND admission.profile=source.profile
 AND admission.source_hash=source.source_hash WHERE source.source_hash=$1`, use.Source).Scan(&raw, &probe.before); err != nil {
			return domain.DocEventDisplayPreparation{}, err
		}
		if domain.HashContent(raw) != use.Source || probe.before != use.Generation {
			return domain.DocEventDisplayPreparation{}, domain.ErrIntegrity
		}
		if probe.catalogEpoch {
			var owned bool
			if err := transaction.QueryRow(ctx, `SELECT current_database()=$1`, probe.database).Scan(&owned); err != nil || !owned || probe.database == "" {
				return domain.DocEventDisplayPreparation{}, errors.Join(domain.ErrIntegrity, err)
			}
			if _, err := transaction.Exec(ctx, `SELECT cxthub_bind_doc_catalog_control($1,$2,$3,$4,$5)`,
				job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version); err != nil {
				return domain.DocEventDisplayPreparation{}, err
			}
			if err := transaction.QueryRow(ctx, `SELECT catalog_epoch::text FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=$1`, use.Source).Scan(&probe.before); err != nil || probe.before != use.Epoch {
				return domain.DocEventDisplayPreparation{}, errors.Join(domain.ErrIntegrity, err)
			}
			var guards string
			if err := transaction.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(tgname,tgenabled::text) ORDER BY tgname)::text
 FROM pg_trigger WHERE tgrelid='doc_display_catalog_lifecycle_v1'::regclass AND tgenabled='O'`).Scan(&guards); err != nil {
				return domain.DocEventDisplayPreparation{}, err
			}
			if _, err := transaction.Exec(ctx, `ALTER TABLE doc_display_catalog_lifecycle_v1 DISABLE TRIGGER doc_display_catalog_lifecycle_v1_immutable`); err != nil {
				return domain.DocEventDisplayPreparation{}, err
			}
			if err := transaction.QueryRow(ctx, `UPDATE doc_display_catalog_lifecycle_v1 SET catalog_epoch=gen_random_uuid()
 WHERE source_hash=$1 RETURNING catalog_epoch::text`, use.Source).Scan(&probe.after); err != nil {
				return domain.DocEventDisplayPreparation{}, err
			}
			if _, err := transaction.Exec(ctx, `ALTER TABLE doc_display_catalog_lifecycle_v1 ENABLE TRIGGER doc_display_catalog_lifecycle_v1_immutable`); err != nil {
				return domain.DocEventDisplayPreparation{}, err
			}
			var restored, generation string
			if err := transaction.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(tgname,tgenabled::text) ORDER BY tgname)::text
 FROM pg_trigger WHERE tgrelid='doc_display_catalog_lifecycle_v1'::regclass`).Scan(&restored); err != nil || restored != guards {
				return domain.DocEventDisplayPreparation{}, errors.Join(domain.ErrIntegrity, err)
			}
			if err := transaction.QueryRow(ctx, `SELECT generation::text FROM doc_display_catalog_admissions_v1 WHERE source_hash=$1`, use.Source).Scan(&generation); err != nil || generation != use.Generation {
				return domain.DocEventDisplayPreparation{}, errors.Join(domain.ErrIntegrity, err)
			}
		} else {
			for _, replacement := range [][]byte{[]byte(`{}`), raw} {
				if tag, err := transaction.Exec(ctx, `UPDATE doc_display_sources_v1 SET bytes=$2 WHERE source_hash=$1`, use.Source, replacement); err != nil || tag.RowsAffected() != 1 {
					return domain.DocEventDisplayPreparation{}, errors.Join(domain.ErrIntegrity, err)
				}
			}
			if err := transaction.QueryRow(ctx, `SELECT generation::text FROM doc_display_catalog_admissions_v1 WHERE source_hash=$1`, use.Source).Scan(&probe.after); err != nil {
				return domain.DocEventDisplayPreparation{}, err
			}
		}
		if probe.after == probe.before {
			return domain.DocEventDisplayPreparation{}, domain.ErrIntegrity
		}
		if err := transaction.Commit(ctx); err != nil {
			return domain.DocEventDisplayPreparation{}, err
		}
		probe.changed = true
	}
	return probe.PostgresStore.SaveDocEventDisplay(ctx, job, step)
}
