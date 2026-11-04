//go:build postgres

package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) InitializeDocDisplay(ctx context.Context, job domain.DocDisplayJob, source domain.DocDisplayStringSource) (domain.DocDisplayPreparation, error) {
	var result domain.DocDisplayPreparation
	err := s.docDisplayWrite(ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
		var err error
		result, err = s.initializeDocDisplay(bound, transaction, job, source)
		return err
	})
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	return result, nil
}

func (storage *PostgresStore) initializeDocDisplay(ctx context.Context, transaction pgx.Tx, job domain.DocDisplayJob, source domain.DocDisplayStringSource) (domain.DocDisplayPreparation, error) {
	raw, err := source.Encode()
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	if source.EventHash != job.Target.Event.Hash || source.EventBytes != job.Target.Event.Length {
		return domain.DocDisplayPreparation{}, domain.ErrIntegrity
	}
	state, err := domain.NewDocDisplayStringState(source)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	stateRaw, err := state.Encode(source)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	prior, err := storage.readDocDisplayPreparation(ctx, job)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	tag, err := transaction.Exec(ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, source.EventHash, source.Profile, state.Source, raw)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	if tag.RowsAffected() == 0 {
		var agrees bool
		if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_display_sources_v1
 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3 AND bytes=$4)`, source.EventHash, source.Profile, state.Source, raw).Scan(&agrees); err != nil {
			return domain.DocDisplayPreparation{}, err
		}
		if !agrees {
			return domain.DocDisplayPreparation{}, domain.ErrIntegrity
		}
	}
	if prior.Checkpoint != "" {
		return prior, nil
	}
	var discoveryHash any
	if prior.Discovery != nil {
		discoveryHash = prior.DiscoveryCheckpoint
	}
	checkpoint := domain.HashContent(stateRaw)
	tag, err = transaction.Exec(ctx, `UPDATE doc_display_jobs_v1 SET source_hash=$4,checkpoint_hash=$5,checkpoint=$6,
 discovery_hash=NULL,discovery=NULL
 WHERE `+docDisplayKeySQL+` AND source_hash IS NULL AND checkpoint_hash IS NULL AND checkpoint IS NULL
 AND discovery_hash IS NOT DISTINCT FROM $8 AND version=$7 AND state='running' AND lease_until>clock_timestamp()`, job.RepoID, job.DocHash, job.Target.Event.Index,
		state.Source, checkpoint, stateRaw, job.Version, discoveryHash)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	if tag.RowsAffected() != 1 {
		return domain.DocDisplayPreparation{}, domain.ErrConflict
	}
	return domain.DocDisplayPreparation{Source: &source, State: state, Checkpoint: checkpoint}, nil
}

func (s *PostgresStore) SaveDocDisplayStep(ctx context.Context, job domain.DocDisplayJob, checkpoint domain.ContentHash, step domain.DocDisplayStringStep) (domain.DocDisplayPreparation, error) {
	if domain.ValidateContentHash(checkpoint) != nil {
		return domain.DocDisplayPreparation{}, domain.ErrValidation
	}
	var result domain.DocDisplayPreparation
	err := s.docDisplayWrite(ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
		prior, err := s.readDocDisplayPreparation(bound, job)
		if err != nil {
			return err
		}
		if prior.Source == nil || prior.Checkpoint == "" || prior.Checkpoint != checkpoint {
			return domain.ErrConflict
		}
		if err := domain.ValidateDocDisplayStringStep(bound, *prior.Source, prior.State, step); err != nil {
			return err
		}
		raw, err := step.State.Encode(*prior.Source)
		if err != nil {
			return err
		}
		if err := putDocDisplayObjects(bound, transaction, step.State.Source, step.Objects); err != nil {
			return err
		}
		next := domain.HashContent(raw)
		tag, err := transaction.Exec(bound, `UPDATE doc_display_jobs_v1 SET checkpoint_hash=$5,checkpoint=$6
 WHERE `+docDisplayKeySQL+` AND checkpoint_hash=$4 AND source_hash=$7 AND version=$8 AND state='running'
 AND lease_until>clock_timestamp()`, job.RepoID, job.DocHash, job.Target.Event.Index, checkpoint, next, raw, step.State.Source, job.Version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrConflict
		}
		result = domain.DocDisplayPreparation{Source: prior.Source, State: step.State, Checkpoint: next}
		return nil
	})
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	return result, nil
}

func (s *PostgresStore) CompleteDocDisplay(ctx context.Context, job domain.DocDisplayJob, checkpoint domain.ContentHash, now time.Time) error {
	if domain.ValidateContentHash(checkpoint) != nil || now.IsZero() {
		return domain.ErrValidation
	}
	return s.docDisplayWrite(ctx, job, now, func(bound context.Context, transaction pgx.Tx) error {
		prior, err := s.readDocDisplayPreparation(bound, job)
		if err != nil {
			return err
		}
		if prior.Source == nil || prior.Checkpoint == "" || prior.Checkpoint != checkpoint {
			return domain.ErrConflict
		}
		generation, objects, err := domain.FinishDocDisplayString(bound, *prior.Source, prior.State)
		if err != nil {
			return err
		}
		raw, err := generation.Encode()
		if err != nil {
			return err
		}
		if err := putDocDisplayObjects(bound, transaction, prior.State.Source, objects); err != nil {
			return err
		}
		hash := domain.HashContent(raw)
		tag, err := transaction.Exec(bound, `INSERT INTO doc_display_generations_v1(event_hash,profile,source_hash,generation_hash,bytes)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, prior.Source.EventHash, prior.Source.Profile, prior.State.Source, hash, raw)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var agrees bool
			if err := transaction.QueryRow(bound, `SELECT EXISTS(SELECT 1 FROM doc_display_generations_v1
 WHERE event_hash=$1 AND profile=$2 AND source_hash=$3 AND generation_hash=$4 AND bytes=$5)`,
				prior.Source.EventHash, prior.Source.Profile, prior.State.Source, hash, raw).Scan(&agrees); err != nil {
				return err
			}
			if !agrees {
				return domain.ErrIntegrity
			}
		}
		return nil
	})
}
