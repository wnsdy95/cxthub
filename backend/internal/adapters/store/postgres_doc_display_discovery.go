//go:build postgres

package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (storage *PostgresStore) SaveDocDisplayDiscovery(ctx context.Context, job domain.DocDisplayJob, step domain.DocFieldDiscoveryStep) (domain.DocDisplayPreparation, error) {
	state, err := step.State()
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	if state.EventHash != job.Target.Event.Hash || state.EventBytes != job.Target.Event.Length {
		return domain.DocDisplayPreparation{}, domain.ErrIntegrity
	}
	hash, raw := step.Checkpoint()
	var result domain.DocDisplayPreparation
	err = storage.docDisplayWrite(ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
		prior, err := storage.readDocDisplayPreparation(bound, job)
		if err != nil {
			return err
		}
		if prior.Checkpoint != "" {
			return domain.ErrConflict
		}
		var previous any
		if prior.Discovery == nil {
			if step.From() != 0 || step.Previous() != "" {
				return domain.ErrConflict
			}
		} else {
			if step.From() != prior.Discovery.Next || step.Previous() != prior.DiscoveryCheckpoint {
				return domain.ErrConflict
			}
			previous = prior.DiscoveryCheckpoint
		}
		if state.Done {
			source, err := state.DisplaySource()
			if err != nil {
				return err
			}
			result, err = storage.initializeDocDisplay(bound, transaction, job, source)
			return err
		}
		tag, err := transaction.Exec(bound, `UPDATE doc_display_jobs_v1 SET discovery_hash=$4,discovery=$5
 WHERE `+docDisplayKeySQL+` AND discovery_hash IS NOT DISTINCT FROM $6
 AND source_hash IS NULL AND checkpoint_hash IS NULL AND checkpoint IS NULL
 AND version=$7 AND state='running' AND lease_until>clock_timestamp()`,
			job.RepoID, job.DocHash, job.Target.Event.Index, hash, raw, previous, job.Version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrConflict
		}
		result = domain.DocDisplayPreparation{Discovery: &state, DiscoveryCheckpoint: hash, Source: prior.Source}
		return nil
	})
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	return result, nil
}
