//go:build postgres

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docDisplayCatalogCensus struct {
	generation, epoch, phase, reason string
	admission, revision              int64
	repo, doc, profile               string
	event                            int
}

func censusDocDisplayCatalog(ctx context.Context, transaction pgx.Tx, source domain.DocDisplayCatalogSource, epoch string, revision int64, result *domain.DocDisplayMaintenanceStep) (bool, error) {
	if err := source.Validate(); err != nil {
		return false, err
	}
	if domain.ValidateDocDisplayCatalogEpoch(epoch) != nil {
		return false, domain.ErrIntegrity
	}
	if _, err := transaction.Exec(ctx, `SELECT * FROM cxthub_lock_doc_catalog_affinities(ARRAY[$1]::text[],ARRAY[$2]::text[],ARRAY[]::text[])`, source.EventHash, source.Profile); err != nil {
		return false, err
	}
	var generation, bound string
	var admission int64
	if err := transaction.QueryRow(ctx, `SELECT generation::text,admission_revision,coalesce(source_hash,'')
 FROM doc_display_catalog_admissions_v1 WHERE event_hash=$1 AND profile=$2`, source.EventHash, source.Profile).Scan(&generation, &admission, &bound); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, domain.ErrIntegrity
		}
		return false, err
	}
	if bound != string(source.Hash) || generation == "" || admission < 0 || revision < 0 {
		return false, domain.ErrIntegrity
	}
	state := docDisplayCatalogCensus{}
	err := transaction.QueryRow(ctx, `SELECT generation::text,catalog_epoch::text,admission_revision,source_revision,phase,reason,after_repo,after_doc,after_event,after_profile
 FROM doc_display_catalog_census_v1 WHERE source_hash=$1`, source.Hash).Scan(&state.generation, &state.epoch, &state.admission, &state.revision, &state.phase, &state.reason, &state.repo, &state.doc, &state.event, &state.profile)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if errors.Is(err, pgx.ErrNoRows) || state.generation != generation || state.epoch != epoch || state.admission != admission || state.revision != revision {
		state = docDisplayCatalogCensus{generation: generation, epoch: epoch, admission: admission, revision: revision, phase: "native", event: -1}
	}
	result.CatalogCensus = state.phase
	if state.phase == "protected" || state.phase == "clear" {
		return false, nil
	}
	if state.phase != "native" && state.phase != "explicit" {
		return false, domain.ErrIntegrity
	}
	predicate := `event_hash=$1 AND profile=$2`
	if state.phase == "explicit" {
		predicate = `source_hash=$3`
	}
	rows, err := transaction.Query(ctx, `SELECT repo_id,doc_hash,event_index,profile,event_hash,event_length,state,worker_protocol,
 (discovery_hash IS NOT NULL OR discovery IS NOT NULL),coalesce(source_hash,''),
 coalesce(checkpoint_hash,''),CASE WHEN state='completed' AND event_hash=$1 AND profile=$2 AND source_hash=$3 AND profile=$9 THEN substring(checkpoint FROM 1 FOR $10) END,
 coalesce(preparation_hash,''),CASE WHEN state='completed' AND event_hash=$1 AND profile=$2 AND source_hash=$3 AND profile=$11 THEN substring(preparation FROM 1 FOR $12) END
 FROM doc_display_jobs_v1 WHERE `+predicate+` AND (repo_id,doc_hash,event_index,profile)>($4,$5,$6,$7)
 ORDER BY repo_id,doc_hash,event_index,profile LIMIT $8`, source.EventHash, source.Profile, source.Hash,
		state.repo, state.doc, state.event, state.profile, domain.DocDisplayCatalogCensusBatch,
		domain.DocDisplayStringProfile, domain.MaxDocDisplayStateBytes+1, domain.DocDisplayProjectionProfile, domain.MaxDocEventDisplayPreparationBytes+1)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var receipt domain.DocDisplayCatalogReceipt
		if err := rows.Scan(&state.repo, &state.doc, &state.event, &state.profile, &receipt.EventHash, &receipt.EventBytes, &receipt.State, &receipt.WorkerProtocol, &receipt.Discovering, &receipt.Source,
			&receipt.CheckpointHash, &receipt.Checkpoint, &receipt.PreparationHash, &receipt.Preparation); err != nil {
			rows.Close()
			return false, err
		}
		receipt.Profile = state.profile
		result.CatalogJobs++
		if reason := receipt.CatalogProtection(source); reason != "" {
			state.phase, state.reason = "protected", reason
			break
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if state.phase != "protected" && result.CatalogJobs < domain.DocDisplayCatalogCensusBatch {
		if state.phase == "native" {
			state.phase = "explicit"
			state.repo, state.doc, state.event, state.profile = "", "", -1, ""
		} else {
			state.phase = "clear"
		}
	}
	_, err = transaction.Exec(ctx, `INSERT INTO doc_display_catalog_census_v1
 (source_hash,generation,admission_revision,source_revision,phase,reason,after_repo,after_doc,after_event,after_profile,catalog_epoch)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::uuid) ON CONFLICT(source_hash) DO UPDATE SET
 generation=EXCLUDED.generation,catalog_epoch=EXCLUDED.catalog_epoch,admission_revision=EXCLUDED.admission_revision,source_revision=EXCLUDED.source_revision,
 phase=EXCLUDED.phase,reason=EXCLUDED.reason,after_repo=EXCLUDED.after_repo,after_doc=EXCLUDED.after_doc,after_event=EXCLUDED.after_event,after_profile=EXCLUDED.after_profile`,
		source.Hash, state.generation, state.admission, state.revision, state.phase, state.reason, state.repo, state.doc, state.event, state.profile, state.epoch)
	result.CatalogCensus = state.phase
	return true, err
}
